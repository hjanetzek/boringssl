// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runner

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/subtle"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
)

type clientHandshakeState struct {
	c             *Conn
	serverHello   *serverHelloMsg
	hello         *clientHelloMsg
	suite         *cipherSuite
	finishedHash  finishedHash
	keyShares     map[CurveID]ecdhCurve
	masterSecret  []byte
	session       *ClientSessionState
	finishedBytes []byte
}

func (c *Conn) clientHandshake() error {
	if c.config == nil {
		c.config = defaultConfig()
	}

	if len(c.config.ServerName) == 0 && !c.config.InsecureSkipVerify {
		return errors.New("tls: either ServerName or InsecureSkipVerify must be specified in the tls.Config")
	}

	c.sendHandshakeSeq = 0
	c.recvHandshakeSeq = 0

	nextProtosLength := 0
	for _, proto := range c.config.NextProtos {
		if l := len(proto); l > 255 {
			return errors.New("tls: invalid NextProtos value")
		} else {
			nextProtosLength += 1 + l
		}
	}
	if nextProtosLength > 0xffff {
		return errors.New("tls: NextProtos values too large")
	}

	hello := &clientHelloMsg{
		isDTLS:                  c.isDTLS,
		vers:                    c.config.maxVersion(c.isDTLS),
		compressionMethods:      []uint8{compressionNone},
		random:                  make([]byte, 32),
		ocspStapling:            true,
		sctListSupported:        true,
		serverName:              c.config.ServerName,
		supportedCurves:         c.config.curvePreferences(),
		supportedPoints:         []uint8{pointFormatUncompressed},
		nextProtoNeg:            len(c.config.NextProtos) > 0,
		secureRenegotiation:     []byte{},
		alpnProtocols:           c.config.NextProtos,
		duplicateExtension:      c.config.Bugs.DuplicateExtension,
		channelIDSupported:      c.config.ChannelID != nil,
		npnLast:                 c.config.Bugs.SwapNPNAndALPN,
		extendedMasterSecret:    c.config.maxVersion(c.isDTLS) >= VersionTLS10,
		srtpProtectionProfiles:  c.config.SRTPProtectionProfiles,
		srtpMasterKeyIdentifier: c.config.Bugs.SRTPMasterKeyIdentifer,
		customExtension:         c.config.Bugs.CustomExtension,
	}

	if c.config.Bugs.SendClientVersion != 0 {
		hello.vers = c.config.Bugs.SendClientVersion
	}

	if c.config.Bugs.NoExtendedMasterSecret {
		hello.extendedMasterSecret = false
	}

	if c.config.Bugs.NoSupportedCurves {
		hello.supportedCurves = nil
	}

	if len(c.clientVerify) > 0 && !c.config.Bugs.EmptyRenegotiationInfo {
		if c.config.Bugs.BadRenegotiationInfo {
			hello.secureRenegotiation = append(hello.secureRenegotiation, c.clientVerify...)
			hello.secureRenegotiation[0] ^= 0x80
		} else {
			hello.secureRenegotiation = c.clientVerify
		}
	}

	if c.noRenegotiationInfo() {
		hello.secureRenegotiation = nil
	}

	var keyShares map[CurveID]ecdhCurve
	if hello.vers >= VersionTLS13 && enableTLS13Handshake {
		// Offer every supported curve in the initial ClientHello.
		//
		// TODO(davidben): For real code, default to a more conservative
		// set like P-256 and X25519. Make it configurable for tests to
		// stress the HelloRetryRequest logic when implemented.
		keyShares = make(map[CurveID]ecdhCurve)
		for _, curveID := range hello.supportedCurves {
			curve, ok := curveForCurveID(curveID)
			if !ok {
				continue
			}
			publicKey, err := curve.offer(c.config.rand())
			if err != nil {
				return err
			}
			hello.keyShares = append(hello.keyShares, keyShareEntry{
				group:       curveID,
				keyExchange: publicKey,
			})
			keyShares[curveID] = curve
		}
	}

	possibleCipherSuites := c.config.cipherSuites()
	hello.cipherSuites = make([]uint16, 0, len(possibleCipherSuites))

NextCipherSuite:
	for _, suiteId := range possibleCipherSuites {
		for _, suite := range cipherSuites {
			if suite.id != suiteId {
				continue
			}
			if !c.config.Bugs.EnableAllCiphers {
				// Don't advertise TLS 1.2-only cipher suites unless
				// we're attempting TLS 1.2.
				if hello.vers < VersionTLS12 && suite.flags&suiteTLS12 != 0 {
					continue
				}
				// Don't advertise non-DTLS cipher suites in DTLS.
				if c.isDTLS && suite.flags&suiteNoDTLS != 0 {
					continue
				}
			}
			hello.cipherSuites = append(hello.cipherSuites, suiteId)
			continue NextCipherSuite
		}
	}

	if c.config.Bugs.SendRenegotiationSCSV {
		hello.cipherSuites = append(hello.cipherSuites, renegotiationSCSV)
	}

	if c.config.Bugs.SendFallbackSCSV {
		hello.cipherSuites = append(hello.cipherSuites, fallbackSCSV)
	}

	_, err := io.ReadFull(c.config.rand(), hello.random)
	if err != nil {
		c.sendAlert(alertInternalError)
		return errors.New("tls: short read from Rand: " + err.Error())
	}

	if hello.vers >= VersionTLS12 && !c.config.Bugs.NoSignatureAlgorithms {
		hello.signatureAlgorithms = c.config.signatureAlgorithmsForClient()
	}

	var session *ClientSessionState
	var cacheKey string
	sessionCache := c.config.ClientSessionCache

	if sessionCache != nil {
		hello.ticketSupported = !c.config.SessionTicketsDisabled

		// Try to resume a previously negotiated TLS session, if
		// available.
		cacheKey = clientSessionCacheKey(c.conn.RemoteAddr(), c.config)
		candidateSession, ok := sessionCache.Get(cacheKey)
		if ok {
			ticketOk := !c.config.SessionTicketsDisabled || candidateSession.sessionTicket == nil

			// Check that the ciphersuite/version used for the
			// previous session are still valid.
			cipherSuiteOk := false
			for _, id := range hello.cipherSuites {
				if id == candidateSession.cipherSuite {
					cipherSuiteOk = true
					break
				}
			}

			versOk := candidateSession.vers >= c.config.minVersion(c.isDTLS) &&
				candidateSession.vers <= c.config.maxVersion(c.isDTLS)
			if ticketOk && versOk && cipherSuiteOk {
				session = candidateSession
			}
		}
	}

	if session != nil {
		if session.sessionTicket != nil {
			hello.sessionTicket = session.sessionTicket
			if c.config.Bugs.CorruptTicket {
				hello.sessionTicket = make([]byte, len(session.sessionTicket))
				copy(hello.sessionTicket, session.sessionTicket)
				if len(hello.sessionTicket) > 0 {
					offset := 40
					if offset > len(hello.sessionTicket) {
						offset = len(hello.sessionTicket) - 1
					}
					hello.sessionTicket[offset] ^= 0x40
				}
			}
			// A random session ID is used to detect when the
			// server accepted the ticket and is resuming a session
			// (see RFC 5077).
			sessionIdLen := 16
			if c.config.Bugs.OversizedSessionId {
				sessionIdLen = 33
			}
			hello.sessionId = make([]byte, sessionIdLen)
			if _, err := io.ReadFull(c.config.rand(), hello.sessionId); err != nil {
				c.sendAlert(alertInternalError)
				return errors.New("tls: short read from Rand: " + err.Error())
			}
		} else {
			hello.sessionId = session.sessionId
		}
	}

	var helloBytes []byte
	if c.config.Bugs.SendV2ClientHello {
		// Test that the peer left-pads random.
		hello.random[0] = 0
		v2Hello := &v2ClientHelloMsg{
			vers:         hello.vers,
			cipherSuites: hello.cipherSuites,
			// No session resumption for V2ClientHello.
			sessionId: nil,
			challenge: hello.random[1:],
		}
		helloBytes = v2Hello.marshal()
		c.writeV2Record(helloBytes)
	} else {
		helloBytes = hello.marshal()
		c.writeRecord(recordTypeHandshake, helloBytes)
	}
	c.flushHandshake()

	if err := c.simulatePacketLoss(nil); err != nil {
		return err
	}
	msg, err := c.readHandshake()
	if err != nil {
		return err
	}

	if c.isDTLS {
		helloVerifyRequest, ok := msg.(*helloVerifyRequestMsg)
		if ok {
			if helloVerifyRequest.vers != VersionTLS10 {
				// Per RFC 6347, the version field in
				// HelloVerifyRequest SHOULD be always DTLS
				// 1.0. Enforce this for testing purposes.
				return errors.New("dtls: bad HelloVerifyRequest version")
			}

			hello.raw = nil
			hello.cookie = helloVerifyRequest.cookie
			helloBytes = hello.marshal()
			c.writeRecord(recordTypeHandshake, helloBytes)
			c.flushHandshake()

			if err := c.simulatePacketLoss(nil); err != nil {
				return err
			}
			msg, err = c.readHandshake()
			if err != nil {
				return err
			}
		}
	}

	// TODO(davidben): Handle HelloRetryRequest.
	serverHello, ok := msg.(*serverHelloMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(serverHello, msg)
	}

	c.vers, ok = c.config.mutualVersion(serverHello.vers, c.isDTLS)
	if !ok {
		c.sendAlert(alertProtocolVersion)
		return fmt.Errorf("tls: server selected unsupported protocol version %x", serverHello.vers)
	}
	c.haveVers = true

	// Check for downgrade signals in the server random, per
	// draft-ietf-tls-tls13-13, section 6.3.1.2.
	if c.vers <= VersionTLS12 && c.config.maxVersion(c.isDTLS) >= VersionTLS13 {
		if bytes.Equal(serverHello.random[:8], downgradeTLS13) {
			c.sendAlert(alertProtocolVersion)
			return errors.New("tls: downgrade from TLS 1.3 detected")
		}
	}
	if c.vers <= VersionTLS11 && c.config.maxVersion(c.isDTLS) >= VersionTLS12 {
		if bytes.Equal(serverHello.random[:8], downgradeTLS12) {
			c.sendAlert(alertProtocolVersion)
			return errors.New("tls: downgrade from TLS 1.2 detected")
		}
	}

	suite := mutualCipherSuite(c.config.cipherSuites(), serverHello.cipherSuite)
	if suite == nil {
		c.sendAlert(alertHandshakeFailure)
		return fmt.Errorf("tls: server selected an unsupported cipher suite")
	}

	hs := &clientHandshakeState{
		c:            c,
		serverHello:  serverHello,
		hello:        hello,
		suite:        suite,
		finishedHash: newFinishedHash(c.vers, suite),
		keyShares:    keyShares,
		session:      session,
	}

	hs.writeHash(helloBytes, hs.c.sendHandshakeSeq-1)
	hs.writeServerHash(hs.serverHello.marshal())

	if c.vers >= VersionTLS13 && enableTLS13Handshake {
		if err := hs.doTLS13Handshake(); err != nil {
			return err
		}
	} else {
		if c.config.Bugs.EarlyChangeCipherSpec > 0 {
			hs.establishKeys()
			c.writeRecord(recordTypeChangeCipherSpec, []byte{1})
		}

		if hs.serverHello.compressionMethod != compressionNone {
			c.sendAlert(alertUnexpectedMessage)
			return errors.New("tls: server selected unsupported compression format")
		}

		err = hs.processServerExtensions(&serverHello.extensions)
		if err != nil {
			return err
		}

		isResume, err := hs.processServerHello()
		if err != nil {
			return err
		}

		if isResume {
			if c.config.Bugs.EarlyChangeCipherSpec == 0 {
				if err := hs.establishKeys(); err != nil {
					return err
				}
			}
			if err := hs.readSessionTicket(); err != nil {
				return err
			}
			if err := hs.readFinished(c.firstFinished[:]); err != nil {
				return err
			}
			if err := hs.sendFinished(nil, isResume); err != nil {
				return err
			}
		} else {
			if err := hs.doFullHandshake(); err != nil {
				return err
			}
			if err := hs.establishKeys(); err != nil {
				return err
			}
			if err := hs.sendFinished(c.firstFinished[:], isResume); err != nil {
				return err
			}
			// Most retransmits are triggered by a timeout, but the final
			// leg of the handshake is retransmited upon re-receiving a
			// Finished.
			if err := c.simulatePacketLoss(func() {
				c.writeRecord(recordTypeHandshake, hs.finishedBytes)
				c.flushHandshake()
			}); err != nil {
				return err
			}
			if err := hs.readSessionTicket(); err != nil {
				return err
			}
			if err := hs.readFinished(nil); err != nil {
				return err
			}
		}

		if sessionCache != nil && hs.session != nil && session != hs.session {
			if c.config.Bugs.RequireSessionTickets && len(hs.session.sessionTicket) == 0 {
				return errors.New("tls: new session used session IDs instead of tickets")
			}
			sessionCache.Put(cacheKey, hs.session)
		}

		c.didResume = isResume
	}

	c.handshakeComplete = true
	c.cipherSuite = suite
	copy(c.clientRandom[:], hs.hello.random)
	copy(c.serverRandom[:], hs.serverHello.random)
	copy(c.masterSecret[:], hs.masterSecret)

	return nil
}

func (hs *clientHandshakeState) doTLS13Handshake() error {
	c := hs.c

	// Once the PRF hash is known, TLS 1.3 does not require a handshake
	// buffer.
	hs.finishedHash.discardHandshakeBuffer()

	zeroSecret := hs.finishedHash.zeroSecret()

	// Resolve PSK and compute the early secret.
	//
	// TODO(davidben): This will need to be handled slightly earlier once
	// 0-RTT is implemented.
	var psk []byte
	if hs.suite.flags&suitePSK != 0 {
		if !hs.serverHello.hasPSKIdentity {
			c.sendAlert(alertMissingExtension)
			return errors.New("tls: server omitted the PSK identity extension")
		}

		// TODO(davidben): Support PSK ciphers and PSK resumption. Set
		// the resumption context appropriately if resuming.
		return errors.New("tls: PSK ciphers not implemented for TLS 1.3")
	} else {
		if hs.serverHello.hasPSKIdentity {
			c.sendAlert(alertUnsupportedExtension)
			return errors.New("tls: server sent unexpected PSK identity")
		}

		psk = zeroSecret
		hs.finishedHash.setResumptionContext(zeroSecret)
	}

	earlySecret := hs.finishedHash.extractKey(zeroSecret, psk)

	// Resolve ECDHE and compute the handshake secret.
	var ecdheSecret []byte
	if hs.suite.flags&suiteECDHE != 0 {
		if !hs.serverHello.hasKeyShare {
			c.sendAlert(alertMissingExtension)
			return errors.New("tls: server omitted the key share extension")
		}

		curve, ok := hs.keyShares[hs.serverHello.keyShare.group]
		if !ok {
			c.sendAlert(alertHandshakeFailure)
			return errors.New("tls: server selected an unsupported group")
		}

		var err error
		ecdheSecret, err = curve.finish(hs.serverHello.keyShare.keyExchange)
		if err != nil {
			return err
		}
	} else {
		if hs.serverHello.hasKeyShare {
			c.sendAlert(alertUnsupportedExtension)
			return errors.New("tls: server sent unexpected key share extension")
		}

		ecdheSecret = zeroSecret
	}

	// Compute the handshake secret.
	handshakeSecret := hs.finishedHash.extractKey(earlySecret, ecdheSecret)

	// Switch to handshake traffic keys.
	handshakeTrafficSecret := hs.finishedHash.deriveSecret(handshakeSecret, handshakeTrafficLabel)
	c.out.updateKeys(deriveTrafficAEAD(c.vers, hs.suite, handshakeTrafficSecret, handshakePhase, clientWrite), c.vers)
	c.in.updateKeys(deriveTrafficAEAD(c.vers, hs.suite, handshakeTrafficSecret, handshakePhase, serverWrite), c.vers)

	msg, err := c.readHandshake()
	if err != nil {
		return err
	}

	encryptedExtensions, ok := msg.(*encryptedExtensionsMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(encryptedExtensions, msg)
	}
	hs.writeServerHash(encryptedExtensions.marshal())

	err = hs.processServerExtensions(&encryptedExtensions.extensions)
	if err != nil {
		return err
	}

	var chainToSend *Certificate
	var certRequested bool
	var certRequestContext []byte
	if hs.suite.flags&suitePSK != 0 {
		if encryptedExtensions.extensions.ocspResponse != nil {
			c.sendAlert(alertUnsupportedExtension)
			return errors.New("tls: server sent OCSP response without a certificate")
		}
		if encryptedExtensions.extensions.sctList != nil {
			c.sendAlert(alertUnsupportedExtension)
			return errors.New("tls: server sent SCT list without a certificate")
		}
	} else {
		c.ocspResponse = encryptedExtensions.extensions.ocspResponse
		c.sctList = encryptedExtensions.extensions.sctList

		msg, err := c.readHandshake()
		if err != nil {
			return err
		}

		certReq, ok := msg.(*certificateRequestMsg)
		if ok {
			hs.writeServerHash(certReq.marshal())
			certRequested = true
			certRequestContext = certReq.requestContext

			chainToSend, err = selectClientCertificate(c, certReq)
			if err != nil {
				return err
			}

			msg, err = c.readHandshake()
			if err != nil {
				return err
			}
		}

		certMsg, ok := msg.(*certificateMsg)
		if !ok {
			c.sendAlert(alertUnexpectedMessage)
			return unexpectedMessageError(certMsg, msg)
		}
		hs.writeServerHash(certMsg.marshal())

		if err := hs.verifyCertificates(certMsg); err != nil {
			return err
		}
		leaf := c.peerCertificates[0]

		msg, err = c.readHandshake()
		if err != nil {
			return err
		}
		certVerifyMsg, ok := msg.(*certificateVerifyMsg)
		if !ok {
			c.sendAlert(alertUnexpectedMessage)
			return unexpectedMessageError(certVerifyMsg, msg)
		}

		input := hs.finishedHash.certificateVerifyInput(serverCertificateVerifyContextTLS13)
		err = verifyMessage(c.vers, leaf.PublicKey, c.config, certVerifyMsg.signatureAlgorithm, input, certVerifyMsg.signature)
		if err != nil {
			return err
		}

		hs.writeServerHash(certVerifyMsg.marshal())
	}

	msg, err = c.readHandshake()
	if err != nil {
		return err
	}
	serverFinished, ok := msg.(*finishedMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(serverFinished, msg)
	}

	verify := hs.finishedHash.serverSum(handshakeTrafficSecret)
	if len(verify) != len(serverFinished.verifyData) ||
		subtle.ConstantTimeCompare(verify, serverFinished.verifyData) != 1 {
		c.sendAlert(alertHandshakeFailure)
		return errors.New("tls: server's Finished message was incorrect")
	}

	hs.writeServerHash(serverFinished.marshal())

	// The various secrets do not incorporate the client's final leg, so
	// derive them now before updating the handshake context.
	masterSecret := hs.finishedHash.extractKey(handshakeSecret, zeroSecret)
	trafficSecret := hs.finishedHash.deriveSecret(masterSecret, applicationTrafficLabel)

	if certRequested {
		_ = chainToSend
		_ = certRequestContext
		return errors.New("tls: client auth not implemented.")
	}

	// Send a client Finished message.
	finished := new(finishedMsg)
	finished.verifyData = hs.finishedHash.clientSum(handshakeTrafficSecret)
	if c.config.Bugs.BadFinished {
		finished.verifyData[0]++
	}
	c.writeRecord(recordTypeHandshake, finished.marshal())
	c.flushHandshake()

	// Switch to application data keys.
	c.out.updateKeys(deriveTrafficAEAD(c.vers, hs.suite, trafficSecret, applicationPhase, clientWrite), c.vers)
	c.in.updateKeys(deriveTrafficAEAD(c.vers, hs.suite, trafficSecret, applicationPhase, serverWrite), c.vers)

	// TODO(davidben): Derive and save the exporter master secret for key exporters. Swap out the masterSecret field.
	// TODO(davidben): Derive and save the resumption master secret for receiving tickets.
	// TODO(davidben): Save the traffic secret for KeyUpdate.
	return nil
}

func (hs *clientHandshakeState) doFullHandshake() error {
	c := hs.c

	var leaf *x509.Certificate
	if hs.suite.flags&suitePSK == 0 {
		msg, err := c.readHandshake()
		if err != nil {
			return err
		}

		certMsg, ok := msg.(*certificateMsg)
		if !ok {
			c.sendAlert(alertUnexpectedMessage)
			return unexpectedMessageError(certMsg, msg)
		}
		hs.writeServerHash(certMsg.marshal())

		if err := hs.verifyCertificates(certMsg); err != nil {
			return err
		}
		leaf = c.peerCertificates[0]
	}

	if hs.serverHello.extensions.ocspStapling {
		msg, err := c.readHandshake()
		if err != nil {
			return err
		}
		cs, ok := msg.(*certificateStatusMsg)
		if !ok {
			c.sendAlert(alertUnexpectedMessage)
			return unexpectedMessageError(cs, msg)
		}
		hs.writeServerHash(cs.marshal())

		if cs.statusType == statusTypeOCSP {
			c.ocspResponse = cs.response
		}
	}

	msg, err := c.readHandshake()
	if err != nil {
		return err
	}

	keyAgreement := hs.suite.ka(c.vers)

	skx, ok := msg.(*serverKeyExchangeMsg)
	if ok {
		hs.writeServerHash(skx.marshal())
		err = keyAgreement.processServerKeyExchange(c.config, hs.hello, hs.serverHello, leaf, skx)
		if err != nil {
			c.sendAlert(alertUnexpectedMessage)
			return err
		}

		c.peerSignatureAlgorithm = keyAgreement.peerSignatureAlgorithm()

		msg, err = c.readHandshake()
		if err != nil {
			return err
		}
	}

	var chainToSend *Certificate
	var certRequested bool
	certReq, ok := msg.(*certificateRequestMsg)
	if ok {
		certRequested = true

		hs.writeServerHash(certReq.marshal())

		chainToSend, err = selectClientCertificate(c, certReq)
		if err != nil {
			return err
		}

		msg, err = c.readHandshake()
		if err != nil {
			return err
		}
	}

	shd, ok := msg.(*serverHelloDoneMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(shd, msg)
	}
	hs.writeServerHash(shd.marshal())

	// If the server requested a certificate then we have to send a
	// Certificate message in TLS, even if it's empty because we don't have
	// a certificate to send. In SSL 3.0, skip the message and send a
	// no_certificate warning alert.
	if certRequested {
		if c.vers == VersionSSL30 && chainToSend == nil {
			c.sendAlert(alertNoCertficate)
		} else if !c.config.Bugs.SkipClientCertificate {
			certMsg := new(certificateMsg)
			if chainToSend != nil {
				certMsg.certificates = chainToSend.Certificate
			}
			hs.writeClientHash(certMsg.marshal())
			c.writeRecord(recordTypeHandshake, certMsg.marshal())
		}
	}

	preMasterSecret, ckx, err := keyAgreement.generateClientKeyExchange(c.config, hs.hello, leaf)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}
	if ckx != nil {
		if c.config.Bugs.EarlyChangeCipherSpec < 2 {
			hs.writeClientHash(ckx.marshal())
		}
		c.writeRecord(recordTypeHandshake, ckx.marshal())
	}

	if hs.serverHello.extensions.extendedMasterSecret && c.vers >= VersionTLS10 {
		hs.masterSecret = extendedMasterFromPreMasterSecret(c.vers, hs.suite, preMasterSecret, hs.finishedHash)
		c.extendedMasterSecret = true
	} else {
		if c.config.Bugs.RequireExtendedMasterSecret {
			return errors.New("tls: extended master secret required but not supported by peer")
		}
		hs.masterSecret = masterFromPreMasterSecret(c.vers, hs.suite, preMasterSecret, hs.hello.random, hs.serverHello.random)
	}

	if chainToSend != nil {
		certVerify := &certificateVerifyMsg{
			hasSignatureAlgorithm: c.vers >= VersionTLS12,
		}

		// Determine the hash to sign.
		privKey := c.config.Certificates[0].PrivateKey
		if c.config.Bugs.IgnorePeerSignatureAlgorithmPreferences {
			certReq.signatureAlgorithms = c.config.signatureAlgorithmsForClient()
		}

		if certVerify.hasSignatureAlgorithm {
			certVerify.signatureAlgorithm, err = selectSignatureAlgorithm(c.vers, privKey, c.config, certReq.signatureAlgorithms, c.config.signatureAlgorithmsForClient())
			if err != nil {
				c.sendAlert(alertInternalError)
				return err
			}
		}

		if c.vers > VersionSSL30 {
			msg := hs.finishedHash.buffer
			if c.config.Bugs.InvalidCertVerifySignature {
				msg = make([]byte, len(hs.finishedHash.buffer))
				copy(msg, hs.finishedHash.buffer)
				msg[0] ^= 0x80
			}
			certVerify.signature, err = signMessage(c.vers, privKey, c.config, certVerify.signatureAlgorithm, msg)
			if err == nil && c.config.Bugs.SendSignatureAlgorithm != 0 {
				certVerify.signatureAlgorithm = c.config.Bugs.SendSignatureAlgorithm
			}
		} else {
			// SSL 3.0's client certificate construction is
			// incompatible with signatureAlgorithm.
			rsaKey, ok := privKey.(*rsa.PrivateKey)
			if !ok {
				err = errors.New("unsupported signature type for client certificate")
			} else {
				digest := hs.finishedHash.hashForClientCertificateSSL3(hs.masterSecret)
				if c.config.Bugs.InvalidCertVerifySignature {
					digest[0] ^= 0x80
				}
				certVerify.signature, err = rsa.SignPKCS1v15(c.config.rand(), rsaKey, crypto.MD5SHA1, digest)
			}
		}
		if err != nil {
			c.sendAlert(alertInternalError)
			return errors.New("tls: failed to sign handshake with client certificate: " + err.Error())
		}

		hs.writeClientHash(certVerify.marshal())
		c.writeRecord(recordTypeHandshake, certVerify.marshal())
	}
	// flushHandshake will be called in sendFinished.

	hs.finishedHash.discardHandshakeBuffer()

	return nil
}

func (hs *clientHandshakeState) verifyCertificates(certMsg *certificateMsg) error {
	c := hs.c

	if len(certMsg.certificates) == 0 {
		c.sendAlert(alertIllegalParameter)
		return errors.New("tls: no certificates sent")
	}

	certs := make([]*x509.Certificate, len(certMsg.certificates))
	for i, asn1Data := range certMsg.certificates {
		cert, err := x509.ParseCertificate(asn1Data)
		if err != nil {
			c.sendAlert(alertBadCertificate)
			return errors.New("tls: failed to parse certificate from server: " + err.Error())
		}
		certs[i] = cert
	}

	if !c.config.InsecureSkipVerify {
		opts := x509.VerifyOptions{
			Roots:         c.config.RootCAs,
			CurrentTime:   c.config.time(),
			DNSName:       c.config.ServerName,
			Intermediates: x509.NewCertPool(),
		}

		for i, cert := range certs {
			if i == 0 {
				continue
			}
			opts.Intermediates.AddCert(cert)
		}
		var err error
		c.verifiedChains, err = certs[0].Verify(opts)
		if err != nil {
			c.sendAlert(alertBadCertificate)
			return err
		}
	}

	switch certs[0].PublicKey.(type) {
	case *rsa.PublicKey, *ecdsa.PublicKey:
		break
	default:
		c.sendAlert(alertUnsupportedCertificate)
		return fmt.Errorf("tls: server's certificate contains an unsupported type of public key: %T", certs[0].PublicKey)
	}

	c.peerCertificates = certs
	return nil
}

func (hs *clientHandshakeState) establishKeys() error {
	c := hs.c

	clientMAC, serverMAC, clientKey, serverKey, clientIV, serverIV :=
		keysFromMasterSecret(c.vers, hs.suite, hs.masterSecret, hs.hello.random, hs.serverHello.random, hs.suite.macLen, hs.suite.keyLen, hs.suite.ivLen(c.vers))
	var clientCipher, serverCipher interface{}
	var clientHash, serverHash macFunction
	if hs.suite.cipher != nil {
		clientCipher = hs.suite.cipher(clientKey, clientIV, false /* not for reading */)
		clientHash = hs.suite.mac(c.vers, clientMAC)
		serverCipher = hs.suite.cipher(serverKey, serverIV, true /* for reading */)
		serverHash = hs.suite.mac(c.vers, serverMAC)
	} else {
		clientCipher = hs.suite.aead(c.vers, clientKey, clientIV)
		serverCipher = hs.suite.aead(c.vers, serverKey, serverIV)
	}

	c.in.prepareCipherSpec(c.vers, serverCipher, serverHash)
	c.out.prepareCipherSpec(c.vers, clientCipher, clientHash)
	return nil
}

func (hs *clientHandshakeState) processServerExtensions(serverExtensions *serverExtensions) error {
	c := hs.c

	if c.vers < VersionTLS13 || !enableTLS13Handshake {
		if c.config.Bugs.RequireRenegotiationInfo && serverExtensions.secureRenegotiation == nil {
			return errors.New("tls: renegotiation extension missing")
		}

		if len(c.clientVerify) > 0 && !c.noRenegotiationInfo() {
			var expectedRenegInfo []byte
			expectedRenegInfo = append(expectedRenegInfo, c.clientVerify...)
			expectedRenegInfo = append(expectedRenegInfo, c.serverVerify...)
			if !bytes.Equal(serverExtensions.secureRenegotiation, expectedRenegInfo) {
				c.sendAlert(alertHandshakeFailure)
				return fmt.Errorf("tls: renegotiation mismatch")
			}
		}
	}

	if expected := c.config.Bugs.ExpectedCustomExtension; expected != nil {
		if serverExtensions.customExtension != *expected {
			return fmt.Errorf("tls: bad custom extension contents %q", serverExtensions.customExtension)
		}
	}

	clientDidNPN := hs.hello.nextProtoNeg
	clientDidALPN := len(hs.hello.alpnProtocols) > 0
	serverHasNPN := serverExtensions.nextProtoNeg
	serverHasALPN := len(serverExtensions.alpnProtocol) > 0

	if !clientDidNPN && serverHasNPN {
		c.sendAlert(alertHandshakeFailure)
		return errors.New("server advertised unrequested NPN extension")
	}

	if !clientDidALPN && serverHasALPN {
		c.sendAlert(alertHandshakeFailure)
		return errors.New("server advertised unrequested ALPN extension")
	}

	if serverHasNPN && serverHasALPN {
		c.sendAlert(alertHandshakeFailure)
		return errors.New("server advertised both NPN and ALPN extensions")
	}

	if serverHasALPN {
		c.clientProtocol = serverExtensions.alpnProtocol
		c.clientProtocolFallback = false
		c.usedALPN = true
	}

	if serverHasNPN && c.vers >= VersionTLS13 && enableTLS13Handshake {
		c.sendAlert(alertHandshakeFailure)
		return errors.New("server advertised NPN over TLS 1.3")
	}

	if !hs.hello.channelIDSupported && serverExtensions.channelIDRequested {
		c.sendAlert(alertHandshakeFailure)
		return errors.New("server advertised unrequested Channel ID extension")
	}

	if serverExtensions.channelIDRequested && c.vers >= VersionTLS13 && enableTLS13Handshake {
		c.sendAlert(alertHandshakeFailure)
		return errors.New("server advertised Channel ID over TLS 1.3")
	}

	if serverExtensions.srtpProtectionProfile != 0 {
		if serverExtensions.srtpMasterKeyIdentifier != "" {
			return errors.New("tls: server selected SRTP MKI value")
		}

		found := false
		for _, p := range c.config.SRTPProtectionProfiles {
			if p == serverExtensions.srtpProtectionProfile {
				found = true
				break
			}
		}
		if !found {
			return errors.New("tls: server advertised unsupported SRTP profile")
		}

		c.srtpProtectionProfile = serverExtensions.srtpProtectionProfile
	}

	return nil
}

func (hs *clientHandshakeState) serverResumedSession() bool {
	// If the server responded with the same sessionId then it means the
	// sessionTicket is being used to resume a TLS session.
	return hs.session != nil && hs.hello.sessionId != nil &&
		bytes.Equal(hs.serverHello.sessionId, hs.hello.sessionId)
}

func (hs *clientHandshakeState) processServerHello() (bool, error) {
	c := hs.c

	if hs.serverResumedSession() {
		// For test purposes, assert that the server never accepts the
		// resumption offer on renegotiation.
		if c.cipherSuite != nil && c.config.Bugs.FailIfResumeOnRenego {
			return false, errors.New("tls: server resumed session on renegotiation")
		}

		if hs.serverHello.extensions.sctList != nil {
			return false, errors.New("tls: server sent SCT extension on session resumption")
		}

		if hs.serverHello.extensions.ocspStapling {
			return false, errors.New("tls: server sent OCSP extension on session resumption")
		}

		// Restore masterSecret and peerCerts from previous state
		hs.masterSecret = hs.session.masterSecret
		c.peerCertificates = hs.session.serverCertificates
		c.extendedMasterSecret = hs.session.extendedMasterSecret
		c.sctList = hs.session.sctList
		c.ocspResponse = hs.session.ocspResponse
		hs.finishedHash.discardHandshakeBuffer()
		return true, nil
	}

	if hs.serverHello.extensions.sctList != nil {
		c.sctList = hs.serverHello.extensions.sctList
	}

	return false, nil
}

func (hs *clientHandshakeState) readFinished(out []byte) error {
	c := hs.c

	c.readRecord(recordTypeChangeCipherSpec)
	if err := c.in.error(); err != nil {
		return err
	}

	msg, err := c.readHandshake()
	if err != nil {
		return err
	}
	serverFinished, ok := msg.(*finishedMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(serverFinished, msg)
	}

	if c.config.Bugs.EarlyChangeCipherSpec == 0 {
		verify := hs.finishedHash.serverSum(hs.masterSecret)
		if len(verify) != len(serverFinished.verifyData) ||
			subtle.ConstantTimeCompare(verify, serverFinished.verifyData) != 1 {
			c.sendAlert(alertHandshakeFailure)
			return errors.New("tls: server's Finished message was incorrect")
		}
	}
	c.serverVerify = append(c.serverVerify[:0], serverFinished.verifyData...)
	copy(out, serverFinished.verifyData)
	hs.writeServerHash(serverFinished.marshal())
	return nil
}

func (hs *clientHandshakeState) readSessionTicket() error {
	c := hs.c

	// Create a session with no server identifier. Either a
	// session ID or session ticket will be attached.
	session := &ClientSessionState{
		vers:               c.vers,
		cipherSuite:        hs.suite.id,
		masterSecret:       hs.masterSecret,
		handshakeHash:      hs.finishedHash.server.Sum(nil),
		serverCertificates: c.peerCertificates,
		sctList:            c.sctList,
		ocspResponse:       c.ocspResponse,
	}

	if !hs.serverHello.extensions.ticketSupported {
		if c.config.Bugs.ExpectNewTicket {
			return errors.New("tls: expected new ticket")
		}
		if hs.session == nil && len(hs.serverHello.sessionId) > 0 {
			session.sessionId = hs.serverHello.sessionId
			hs.session = session
		}
		return nil
	}

	if c.vers == VersionSSL30 {
		return errors.New("tls: negotiated session tickets in SSL 3.0")
	}

	msg, err := c.readHandshake()
	if err != nil {
		return err
	}
	sessionTicketMsg, ok := msg.(*newSessionTicketMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(sessionTicketMsg, msg)
	}

	session.sessionTicket = sessionTicketMsg.ticket
	hs.session = session

	hs.writeServerHash(sessionTicketMsg.marshal())

	return nil
}

func (hs *clientHandshakeState) sendFinished(out []byte, isResume bool) error {
	c := hs.c

	var postCCSBytes []byte
	seqno := hs.c.sendHandshakeSeq
	if hs.serverHello.extensions.nextProtoNeg {
		nextProto := new(nextProtoMsg)
		proto, fallback := mutualProtocol(c.config.NextProtos, hs.serverHello.extensions.nextProtos)
		nextProto.proto = proto
		c.clientProtocol = proto
		c.clientProtocolFallback = fallback

		nextProtoBytes := nextProto.marshal()
		hs.writeHash(nextProtoBytes, seqno)
		seqno++
		postCCSBytes = append(postCCSBytes, nextProtoBytes...)
	}

	if hs.serverHello.extensions.channelIDRequested {
		channelIDMsg := new(channelIDMsg)
		if c.config.ChannelID.Curve != elliptic.P256() {
			return fmt.Errorf("tls: Channel ID is not on P-256.")
		}
		var resumeHash []byte
		if isResume {
			resumeHash = hs.session.handshakeHash
		}
		r, s, err := ecdsa.Sign(c.config.rand(), c.config.ChannelID, hs.finishedHash.hashForChannelID(resumeHash))
		if err != nil {
			return err
		}
		channelID := make([]byte, 128)
		writeIntPadded(channelID[0:32], c.config.ChannelID.X)
		writeIntPadded(channelID[32:64], c.config.ChannelID.Y)
		writeIntPadded(channelID[64:96], r)
		writeIntPadded(channelID[96:128], s)
		channelIDMsg.channelID = channelID

		c.channelID = &c.config.ChannelID.PublicKey

		channelIDMsgBytes := channelIDMsg.marshal()
		hs.writeHash(channelIDMsgBytes, seqno)
		seqno++
		postCCSBytes = append(postCCSBytes, channelIDMsgBytes...)
	}

	finished := new(finishedMsg)
	if c.config.Bugs.EarlyChangeCipherSpec == 2 {
		finished.verifyData = hs.finishedHash.clientSum(nil)
	} else {
		finished.verifyData = hs.finishedHash.clientSum(hs.masterSecret)
	}
	copy(out, finished.verifyData)
	if c.config.Bugs.BadFinished {
		finished.verifyData[0]++
	}
	c.clientVerify = append(c.clientVerify[:0], finished.verifyData...)
	hs.finishedBytes = finished.marshal()
	hs.writeHash(hs.finishedBytes, seqno)
	postCCSBytes = append(postCCSBytes, hs.finishedBytes...)

	if c.config.Bugs.FragmentAcrossChangeCipherSpec {
		c.writeRecord(recordTypeHandshake, postCCSBytes[:5])
		postCCSBytes = postCCSBytes[5:]
	}
	c.flushHandshake()

	if !c.config.Bugs.SkipChangeCipherSpec &&
		c.config.Bugs.EarlyChangeCipherSpec == 0 {
		ccs := []byte{1}
		if c.config.Bugs.BadChangeCipherSpec != nil {
			ccs = c.config.Bugs.BadChangeCipherSpec
		}
		c.writeRecord(recordTypeChangeCipherSpec, ccs)
	}

	if c.config.Bugs.AppDataAfterChangeCipherSpec != nil {
		c.writeRecord(recordTypeApplicationData, c.config.Bugs.AppDataAfterChangeCipherSpec)
	}
	if c.config.Bugs.AlertAfterChangeCipherSpec != 0 {
		c.sendAlert(c.config.Bugs.AlertAfterChangeCipherSpec)
		return errors.New("tls: simulating post-CCS alert")
	}

	if !c.config.Bugs.SkipFinished {
		c.writeRecord(recordTypeHandshake, postCCSBytes)
		c.flushHandshake()
	}
	return nil
}

func (hs *clientHandshakeState) writeClientHash(msg []byte) {
	// writeClientHash is called before writeRecord.
	hs.writeHash(msg, hs.c.sendHandshakeSeq)
}

func (hs *clientHandshakeState) writeServerHash(msg []byte) {
	// writeServerHash is called after readHandshake.
	hs.writeHash(msg, hs.c.recvHandshakeSeq-1)
}

func (hs *clientHandshakeState) writeHash(msg []byte, seqno uint16) {
	if hs.c.isDTLS {
		// This is somewhat hacky. DTLS hashes a slightly different format.
		// First, the TLS header.
		hs.finishedHash.Write(msg[:4])
		// Then the sequence number and reassembled fragment offset (always 0).
		hs.finishedHash.Write([]byte{byte(seqno >> 8), byte(seqno), 0, 0, 0})
		// Then the reassembled fragment (always equal to the message length).
		hs.finishedHash.Write(msg[1:4])
		// And then the message body.
		hs.finishedHash.Write(msg[4:])
	} else {
		hs.finishedHash.Write(msg)
	}
}

// selectClientCertificate selects a certificate for use with the given
// certificate, or none if none match. It may return a particular certificate or
// nil on success, or an error on internal error.
func selectClientCertificate(c *Conn, certReq *certificateRequestMsg) (*Certificate, error) {
	// RFC 4346 on the certificateAuthorities field:
	// A list of the distinguished names of acceptable certificate
	// authorities. These distinguished names may specify a desired
	// distinguished name for a root CA or for a subordinate CA; thus, this
	// message can be used to describe both known roots and a desired
	// authorization space. If the certificate_authorities list is empty
	// then the client MAY send any certificate of the appropriate
	// ClientCertificateType, unless there is some external arrangement to
	// the contrary.

	var rsaAvail, ecdsaAvail bool
	if !certReq.hasRequestContext {
		for _, certType := range certReq.certificateTypes {
			switch certType {
			case CertTypeRSASign:
				rsaAvail = true
			case CertTypeECDSASign:
				ecdsaAvail = true
			}
		}
	}

	// We need to search our list of client certs for one
	// where SignatureAlgorithm is RSA and the Issuer is in
	// certReq.certificateAuthorities
findCert:
	for i, chain := range c.config.Certificates {
		if !certReq.hasRequestContext && !rsaAvail && !ecdsaAvail {
			continue
		}

		// Ensure the private key supports one of the advertised
		// signature algorithms.
		if certReq.hasSignatureAlgorithm {
			if _, err := selectSignatureAlgorithm(c.vers, chain.PrivateKey, c.config, certReq.signatureAlgorithms, c.config.signatureAlgorithmsForClient()); err != nil {
				continue
			}
		}

		for j, cert := range chain.Certificate {
			x509Cert := chain.Leaf
			// parse the certificate if this isn't the leaf
			// node, or if chain.Leaf was nil
			if j != 0 || x509Cert == nil {
				var err error
				if x509Cert, err = x509.ParseCertificate(cert); err != nil {
					c.sendAlert(alertInternalError)
					return nil, errors.New("tls: failed to parse client certificate #" + strconv.Itoa(i) + ": " + err.Error())
				}
			}

			if !certReq.hasRequestContext {
				switch {
				case rsaAvail && x509Cert.PublicKeyAlgorithm == x509.RSA:
				case ecdsaAvail && x509Cert.PublicKeyAlgorithm == x509.ECDSA:
				default:
					continue findCert
				}
			}

			if len(certReq.certificateAuthorities) == 0 {
				// They gave us an empty list, so just take the
				// first certificate of valid type from
				// c.config.Certificates.
				return &chain, nil
			}

			for _, ca := range certReq.certificateAuthorities {
				if bytes.Equal(x509Cert.RawIssuer, ca) {
					return &chain, nil
				}
			}
		}
	}

	return nil, nil
}

// clientSessionCacheKey returns a key used to cache sessionTickets that could
// be used to resume previously negotiated TLS sessions with a server.
func clientSessionCacheKey(serverAddr net.Addr, config *Config) string {
	if len(config.ServerName) > 0 {
		return config.ServerName
	}
	return serverAddr.String()
}

// mutualProtocol finds the mutual Next Protocol Negotiation or ALPN protocol
// given list of possible protocols and a list of the preference order. The
// first list must not be empty. It returns the resulting protocol and flag
// indicating if the fallback case was reached.
func mutualProtocol(protos, preferenceProtos []string) (string, bool) {
	for _, s := range preferenceProtos {
		for _, c := range protos {
			if s == c {
				return s, false
			}
		}
	}

	return protos[0], true
}

// writeIntPadded writes x into b, padded up with leading zeros as
// needed.
func writeIntPadded(b []byte, x *big.Int) {
	for i := range b {
		b[i] = 0
	}
	xb := x.Bytes()
	copy(b[len(b)-len(xb):], xb)
}
