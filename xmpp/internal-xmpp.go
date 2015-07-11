/*
Copyright 2015 Google Inc. All rights reserved.

Use of this source code is governed by a BSD-style
license that can be found in the LICENSE file or at
https://developers.google.com/open-source/licenses/bsd
*/

// Package xmpp is the Google Cloud Print XMPP interface.
package xmpp

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
)

const (
	// Dump XMPP XMP conversation to stdout.
	debug = false

	// This is a long-lived, potentially quiet, conversation. Keep it alive!
	netKeepAlive = time.Second * 60

	// Set our own timeout, rather than have the OS or server timeout for us.
	netTimeout = time.Second * 60
)

// Interface with XMPP server.
type internalXMPP struct {
	conn       *tls.Conn
	xmlEncoder *xml.Encoder
	xmlDecoder *xml.Decoder
	fullJID    string

	notifications       chan<- PrinterNotification
	pingIntervalUpdates <-chan time.Duration
	pongs               chan uint8
	nextPingID          uint8
	dead                chan<- struct{}
}

// newInternalXMPP creates a new XMPP connection.
//
// Received XMPP notifications are sent on the notifications channel.
//
// Updates to the ping interval are received on pingIntervalUpdates.
//
// If the connection dies unexpectedly, a message is sent on dead.
func newInternalXMPP(jid, accessToken, proxyName, server string, port uint16, pingTimeout, pingInterval time.Duration, notifications chan<- PrinterNotification, pingIntervalUpdates <-chan time.Duration, dead chan<- struct{}) (*internalXMPP, error) {
	var user, domain string
	if parts := strings.SplitN(jid, "@", 2); len(parts) != 2 {
		return nil, fmt.Errorf("Tried to use invalid XMPP JID: %s", jid)
	} else {
		user = parts[0]
		domain = parts[1]
	}

	// Anyone home?
	conn, err := dial(server, port)
	if err != nil {
		return nil, fmt.Errorf("Failed to dial XMPP service: %s", err)
	}

	var xmlEncoder *xml.Encoder
	var xmlDecoder *xml.Decoder
	if debug {
		t := &tee{conn, conn}
		xmlEncoder = xml.NewEncoder(t)
		xmlDecoder = xml.NewDecoder(t)
	} else {
		xmlEncoder = xml.NewEncoder(conn)
		xmlDecoder = xml.NewDecoder(conn)
	}

	// SASL
	if err = saslHandshake(xmlEncoder, xmlDecoder, domain, user, accessToken); err != nil {
		return nil, fmt.Errorf("Failed to perform XMPP-SASL handshake: %s", err)
	}

	// XMPP
	fullJID, err := xmppHandshake(xmlEncoder, xmlDecoder, domain, proxyName)
	if err != nil {
		return nil, fmt.Errorf("Failed to perform final XMPP handshake: %s", err)
	}

	// Subscribe
	if err = subscribe(xmlEncoder, xmlDecoder, fullJID); err != nil {
		return nil, fmt.Errorf("Failed to subscribe: %s", err)
	}

	x := internalXMPP{
		conn:                conn,
		xmlEncoder:          xmlEncoder,
		xmlDecoder:          xmlDecoder,
		fullJID:             fullJID,
		notifications:       notifications,
		pingIntervalUpdates: pingIntervalUpdates,
		pongs:               make(chan uint8, 10),
		nextPingID:          0,
		dead:                dead,
	}

	// dispatchIncoming signals pingPeriodically to return via dying.
	dying := make(chan struct{})
	go x.dispatchIncoming(dying)
	go x.pingPeriodically(pingTimeout, pingInterval, dying)

	// Check by ping
	if success, err := x.ping(pingTimeout); !success {
		return nil, fmt.Errorf("XMPP conversation started, but initial ping failed: %s", err)
	}

	return &x, nil
}

// Quit causes the XMPP connection to close.
func (x *internalXMPP) Quit() {
	// Trigger dispatchIncoming to return.
	x.conn.Close()
	// dispatchIncoming notifies pingPeriodically via dying channel.
	// pingPeriodically signals death via x.dead channel.
}

func (x *internalXMPP) pingPeriodically(timeout, interval time.Duration, dying <-chan struct{}) {
	t := time.NewTimer(interval)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			if success, err := x.ping(timeout); success {
				t.Reset(interval)
			} else {
				glog.Infof("XMPP ping failed; trying once more: %s", err)
				// Ping failed; give it another try, then restart the XMPP conversation.
				if success, _ := x.ping(timeout); !success {
					x.Quit()
				}
			}
		case interval = <-x.pingIntervalUpdates:
			t.Reset(time.Nanosecond) // Induce ping and interval reset now.
		case <-dying:
			// Signal death externally.
			x.dead <- struct{}{}
			return
		}
	}
}

// dispatchIncoming listens for new XMPP notifications and puts them into
// separate channels, by type of message.
func (x *internalXMPP) dispatchIncoming(dying chan<- struct{}) {
	for {
		// The xml.StartElement tells us what is coming up.
		startElement, err := readStartElement(x.xmlDecoder)
		if err != nil {
			if isXMLErrorClosedConnection(err) {
				break
			}
			glog.Warningf("Failed to read the next start element: %s", err)
			continue
		}

		// Parse the message.
		if startElement.Name.Local == "message" {
			var message struct {
				XMLName xml.Name `xml:"message"`
				Data    string   `xml:"push>data"`
			}

			if err := x.xmlDecoder.DecodeElement(&message, startElement); err != nil {
				if isXMLErrorClosedConnection(err) {
					break
				}
				glog.Warningf("Error while parsing print jobs notification via XMPP: %s", err)
				continue
			}

			messageData, err := base64.StdEncoding.DecodeString(message.Data)
			if err != nil {
				glog.Warningf("Failed to convert XMPP message data from base64: %s", err)
				continue
			}

			messageDataString := string(messageData)
			if strings.ContainsRune(messageDataString, '/') {
				if strings.HasSuffix(messageDataString, "/delete") {
					gcpID := strings.TrimSuffix(messageDataString, "/delete")
					x.notifications <- PrinterNotification{gcpID, PrinterDelete}
				}
				// Ignore other suffixes, like /update_settings.
			} else {
				x.notifications <- PrinterNotification{messageDataString, PrinterNewJobs}
			}

		} else if startElement.Name.Local == "iq" {
			var message struct {
				XMLName xml.Name `xml:"iq"`
				ID      string   `xml:"id,attr"`
				Type    string   `xml:"type,attr"`
			}

			if err := x.xmlDecoder.DecodeElement(&message, startElement); err != nil {
				if isXMLErrorClosedConnection(err) {
					break
				}
				glog.Warningf("Error while parsing XMPP pong: %s", err)
				continue
			}

			pingID, err := strconv.ParseUint(message.ID, 10, 8)
			if err != nil {
				glog.Warningf("Failed to convert XMPP ping ID: %s", err)
				continue
			}
			x.pongs <- uint8(pingID)

		} else {
			glog.Warningf("Unexpected element while waiting for print message: %+v", startElement)
		}
	}

	dying <- struct{}{}
}

// ping sends a ping message and blocks until pong is received.
//
// Returns false if timeout time passes before pong, or on any
// other error. Errors are logged but not returned.
func (x *internalXMPP) ping(timeout time.Duration) (bool, error) {
	var ping struct {
		XMLName xml.Name `xml:"iq"`
		From    string   `xml:"from,attr"`
		To      string   `xml:"to,attr"`
		ID      string   `xml:"id,attr"`
		Type    string   `xml:"type,attr"`
		Ping    struct {
			XMLName xml.Name `xml:"ping"`
			XMLNS   string   `xml:"xmlns,attr"`
		}
	}

	pingID := x.nextPingID
	x.nextPingID++

	ping.From = x.fullJID
	ping.To = "cloudprint.google.com"
	ping.ID = fmt.Sprintf("%d", pingID)
	ping.Type = "get"
	ping.Ping.XMLNS = "urn:xmpp:ping"

	if err := x.xmlEncoder.Encode(&ping); err != nil {
		return false, fmt.Errorf("XMPP ping request failed: %s", err)
	}

	for {
		select {
		case pongID := <-x.pongs:
			if pongID == pingID {
				return true, nil
			}
		case <-time.After(timeout):
			return false, fmt.Errorf("Pong not received after %s", timeout.String())
		}
	}
	panic("unreachable")
}

func dial(server string, port uint16) (*tls.Conn, error) {
	tlsConfig := &tls.Config{
		ServerName: server,
	}
	netDialer := &net.Dialer{
		KeepAlive: netKeepAlive,
		Timeout:   netTimeout,
	}
	addr := fmt.Sprintf("%s:%d", server, port)
	conn, err := tls.DialWithDialer(netDialer, "tcp", addr, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("Failed to connect to XMPP server: %s", err)
	}
	if err = conn.VerifyHostname("talk.google.com"); err != nil {
		return nil, fmt.Errorf("Failed to verify hostname of XMPP server: %s", err)
	}

	return conn, nil
}

func saslHandshake(xmlEncoder *xml.Encoder, xmlDecoder *xml.Decoder, domain, user, accessToken string) error {
	handshake := xml.StartElement{
		Name: xml.Name{"jabber:client", "stream:stream"},
		Attr: []xml.Attr{
			xml.Attr{xml.Name{Local: "to"}, domain},
			xml.Attr{xml.Name{Local: "xml:lang"}, "en"},
			xml.Attr{xml.Name{Local: "version"}, "1.0"},
			xml.Attr{xml.Name{Local: "xmlns:stream"}, "http://etherx.jabber.org/streams"},
		},
	}
	if err := xmlEncoder.EncodeToken(handshake); err != nil {
		return fmt.Errorf("Failed to write SASL handshake: %s", err)
	}
	if err := xmlEncoder.Flush(); err != nil {
		return fmt.Errorf("Failed to flush encoding stream: %s", err)
	}

	if startElement, err := readStartElement(xmlDecoder); err != nil {
		return err
	} else if startElement.Name.Space != "http://etherx.jabber.org/streams" ||
		startElement.Name.Local != "stream" {
		return fmt.Errorf("Read unexpected SASL XML stanza: %s", startElement.Name.Local)
	}

	var features struct {
		XMLName    xml.Name `xml:"http://etherx.jabber.org/streams features"`
		Mechanisms *struct {
			XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-sasl mechanisms"`
		}
	}
	if err := xmlDecoder.Decode(&features); err != nil {
		return fmt.Errorf("Read unexpected SASL XML element: %s", err)
	} else if features.Mechanisms == nil {
		return errors.New("SASL mechanisms missing from handshake")
	}

	credential := base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + accessToken))

	var auth struct {
		XMLName    xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-sasl auth"`
		Mechanism  string   `xml:"mechanism,attr"`
		Service    string   `xml:"auth:service,attr"`
		Allow      string   `xml:"auth:allow-generated-jid,attr"`
		FullBind   string   `xml:"auth:client-uses-full-bind-result,attr"`
		XMLNS      string   `xml:"xmlns:auth,attr"`
		Credential string   `xml:",chardata"`
	}
	auth.Mechanism = "X-OAUTH2"
	auth.Service = "chromiumsync"
	auth.Allow = "true"
	auth.FullBind = "true"
	auth.XMLNS = "http://www.google.com/talk/protocol/auth"
	auth.Credential = credential
	if err := xmlEncoder.Encode(auth); err != nil {
		return fmt.Errorf("Failed to write SASL credentials: %s", err)
	}

	var success struct {
		XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-sasl success"`
	}
	if err := xmlDecoder.Decode(&success); err != nil {
		return fmt.Errorf("Failed to complete SASL handshake: %s", err)
	}

	return nil
}

func xmppHandshake(xmlEncoder *xml.Encoder, xmlDecoder *xml.Decoder, domain, proxyName string) (string, error) {
	handshake := xml.StartElement{
		Name: xml.Name{"jabber:client", "stream:stream"},
		Attr: []xml.Attr{
			xml.Attr{xml.Name{Local: "to"}, domain},
			xml.Attr{xml.Name{Local: "xml:lang"}, "en"},
			xml.Attr{xml.Name{Local: "version"}, "1.0"},
			xml.Attr{xml.Name{Local: "xmlns:stream"}, "http://etherx.jabber.org/streams"},
		},
	}
	if err := xmlEncoder.EncodeToken(handshake); err != nil {
		return "", fmt.Errorf("Failed to write SASL handshake: %s", err)
	}
	if err := xmlEncoder.Flush(); err != nil {
		return "", fmt.Errorf("Failed to flush encoding stream: %s", err)
	}

	if startElement, err := readStartElement(xmlDecoder); err != nil {
		return "", err
	} else if startElement.Name.Space != "http://etherx.jabber.org/streams" ||
		startElement.Name.Local != "stream" {
		return "", fmt.Errorf("Read unexpected XMPP XML stanza: %s", startElement.Name.Local)
	}

	var features struct {
		XMLName xml.Name `xml:"http://etherx.jabber.org/streams features"`
		Bind    *struct {
			XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-bind bind"`
		}
		Session *struct {
			XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-session session"`
		}
	}
	if err := xmlDecoder.Decode(&features); err != nil {
		return "", fmt.Errorf("Read unexpected XMPP XML element: %s", err)
	} else if features.Bind == nil || features.Session == nil {
		return "", errors.New("XMPP bind or session missing from handshake")
	}

	var resource struct {
		XMLName xml.Name `xml:"jabber:client iq"`
		Type    string   `xml:"type,attr"`
		ID      string   `xml:"id,attr"`
		Bind    struct {
			XMLName  xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-bind bind"`
			Resource struct {
				XMLName      xml.Name `xml:"resource"`
				ResourceName string   `xml:",chardata"`
			}
		}
	}
	resource.Type = "set"
	resource.ID = "0"
	resource.Bind.Resource.ResourceName = proxyName
	if err := xmlEncoder.Encode(&resource); err != nil {
		return "", fmt.Errorf("Failed to set resource during XMPP handshake: %s", err)
	}

	var jid struct {
		XMLName xml.Name `xml:"jabber:client iq"`
		Bind    *struct {
			XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-bind bind"`
			JID     string   `xml:"jid"`
		}
	}
	if err := xmlDecoder.Decode(&jid); err != nil {
		return "", err
	} else if jid.Bind == nil || jid.Bind.JID == "" {
		return "", errors.New("Received unexpected XML element during XMPP handshake")
	}

	fullJID := jid.Bind.JID

	var session struct {
		XMLName xml.Name `xml:"jabber:client iq"`
		Type    string   `xml:"type,attr"`
		ID      string   `xml:"id,attr"`
		Session struct {
			XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-session session"`
		}
	}
	session.Type = "set"
	session.ID = "1"
	if err := xmlEncoder.Encode(&session); err != nil {
		return "", fmt.Errorf("Failed to complete XMPP handshake: %s", err)
	}

	var done struct {
		XMLName xml.Name `xml:"jabber:client iq"`
		ID      string   `xml:"id,attr"`
	}
	if err := xmlDecoder.Decode(&done); err != nil {
		return "", err
	} else if done.ID != "1" {
		return "", errors.New("Received unexpected result at end of XMPP handshake")
	}

	return fullJID, nil
}

func subscribe(xmlEncoder *xml.Encoder, xmlDecoder *xml.Decoder, fullJID string) error {
	var bareJID string
	if barePosition := strings.Index(fullJID, "/"); barePosition < 0 {
		return fmt.Errorf("Can't split JID %s", fullJID)
	} else {
		bareJID = fullJID[:barePosition]
	}

	var subscribe struct {
		XMLName   xml.Name `xml:"jabber:client iq"`
		Type      string   `xml:"type,attr"`
		To        string   `xml:"to,attr"`
		ID        string   `xml:"id,attr"`
		Subscribe struct {
			XMLName xml.Name `xml:"google:push subscribe"`
			Item    struct {
				XMLName xml.Name `xml:"item"`
				Channel string   `xml:"channel,attr"`
				From    string   `xml:"from,attr"`
			}
		}
	}
	subscribe.Type = "set"
	subscribe.To = bareJID
	subscribe.ID = "3"
	subscribe.Subscribe.Item.Channel = "cloudprint.google.com"
	subscribe.Subscribe.Item.From = "cloudprint.google.com"
	if err := xmlEncoder.Encode(&subscribe); err != nil {
		return fmt.Errorf("XMPP subscription request failed: %s", err)
	}

	var subscription struct {
		XMLName xml.Name `xml:"jabber:client iq"`
		To      string   `xml:"to,attr"`
		From    string   `xml:"from,attr"`
	}
	if err := xmlDecoder.Decode(&subscription); err != nil {
		return fmt.Errorf("XMPP subscription response invalid: %s", err)
	} else if fullJID != subscription.To || bareJID != subscription.From {
		return errors.New("XMPP subscription failed")
	}

	return nil
}

func readStartElement(d *xml.Decoder) (*xml.StartElement, error) {
	for {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		if startElement, ok := token.(xml.StartElement); ok {
			return &startElement, nil
		}
	}
	panic("unreachable")
}

// isXMLErrorClosedConnection simplifies an xml.Decoder error.
//
// If the error is a variation of "connection closed" then logs a suitable error and returns true.
// Otherwise, returns false.
func isXMLErrorClosedConnection(err error) bool {
	if strings.Contains(err.Error(), "use of closed network connection") {
		glog.Info("XMPP connection was closed")
		return true
	} else if strings.Contains(err.Error(), "connection reset by peer") {
		glog.Info("XMPP connection was forcibly closed by server")
		return true
	} else if err == io.EOF {
		glog.Info("XMPP connection failed")
		return true
	}
	return false
}

type tee struct {
	r io.Reader
	w io.Writer
}

func (t *tee) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	glog.Errorf("read %d %s\n", n, p[0:n])
	return n, err
}

func (t *tee) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	glog.Errorf("wrote %d %s\n", n, p[0:n])
	return n, err
}
