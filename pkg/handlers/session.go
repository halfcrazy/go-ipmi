package handlers

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"

	"github.com/bougou/go-ipmi/pkg/bmc"
)

// IPMI session-management command IDs (NetFn 0x06 App).
const (
	CmdGetChannelAuthCapabilities uint8 = 0x38
	CmdActivateSession            uint8 = 0x3A
	CmdSetSessionPrivilegeLevel   uint8 = 0x3B
	CmdCloseSession               uint8 = 0x3C
)

// RegisterSessionHandlers adds IPMI 1.5 session and v2.0 RAKP handlers to r.
// Open Session and RAKP messages are dispatched differently (they arrive before
// a session exists); see [HandleOpenSession], [HandleRAKP1], [HandleRAKP3].
func RegisterSessionHandlers(r *Registry) {
	r.Register(NetFnAppRequest, CmdGetChannelAuthCapabilities, HandlerFunc(handleGetChannelAuthCaps))
	r.Register(NetFnAppRequest, CmdGetChannelCipherSuites, HandlerFunc(handleGetChannelCipherSuites))
	r.Register(NetFnAppRequest, CmdSetSessionPrivilegeLevel, HandlerFunc(handleSetSessionPrivilegeLevel))
	r.Register(NetFnAppRequest, CmdCloseSession, HandlerFunc(handleCloseSession))
}

// ---------------------------------------------------------------------------
// Get Channel Authentication Capabilities
// ---------------------------------------------------------------------------

// handleGetChannelAuthCaps implements Get Channel Authentication Capabilities (App 0x38).
// Advertises IPMI 1.5 auth types (NONE/MD2/MD5/password/OEM) plus IPMI 2.0/RMCP+
// support so that both ipmitool and goipmi clients proceed to the RMCP+ handshake.
func handleGetChannelAuthCaps(_ context.Context, hctx *HandlerContext, req []byte) ([]byte, CompletionCode, error) {
	if len(req) < 2 {
		return nil, CodeRequestDataTruncated, nil
	}
	// req[0] bits 5:0 = channel number (0x0E = current)
	// req[1] bits 3:0 = requested privilege level

	resp := make([]byte, 8)
	resp[0] = 0x01 // channel number (1 = LAN)
	// resp[1] — auth type support (IPMI spec Table 22-15, byte 3):
	//   bit 7 = IPMI v2.0 extended capabilities available
	//   bit 4 = straight password; bit 2 = MD5; bit 1 = MD2; bit 0 = NONE
	resp[1] = 0x97 // 0b1001_0111: IPMI v2.0 ext + password + MD5 + MD2 + NONE
	// resp[2] — byte 4 of Table 22-15:
	//   bit 5 = KgStatus; bit 2 = Non-Null usernames enabled
	resp[2] = 0x24 // 0b0010_0100
	// resp[3] — extended capabilities (byte 5):
	//   bit 1 = IPMI v2.0 connections supported; bit 0 = IPMI v1.5 supported
	resp[3] = 0x03 // IPMI v2.0 + v1.5 connections
	resp[4] = 0x00 // OEM ID byte 1
	resp[5] = 0x00 // OEM ID byte 2
	resp[6] = 0x00 // OEM ID byte 3
	resp[7] = 0x00 // OEM auxiliary data
	return resp, CodeOK, nil
}

// handleGetChannelCipherSuites implements Get Channel Cipher Suites (App 0x54).
// Returns a single record for cipher suite 3 (RAKP-HMAC-SHA1 + HMAC-SHA1-96 +
// AES-CBC-128) — the suite the server actually supports in its RMCP+ handshake.
func handleGetChannelCipherSuites(_ context.Context, hctx *HandlerContext, req []byte) ([]byte, CompletionCode, error) {
	if len(req) < 2 {
		return nil, CodeRequestDataTruncated, nil
	}
	// Byte 0: channel number + list index (bits 5:0 = channel, bits 7:6 = reserved)
	// Byte 1: payload type (0x00 = IPMI)
	// Response: [channel][cipher suite records...]
	//
	// Standard cipher suite record for suite 3:
	//   0xC0           start-of-record (standard)
	//   0x03           cipher suite ID 3
	//   0x01           auth alg  RAKP-HMAC-SHA1   (tag 00b)
	//   0x41           integ alg HMAC-SHA1-96     (tag 01b)
	//   0x81           crypt alg AES-CBC-128      (tag 10b)
	return []byte{0x01, 0xC0, 0x03, 0x01, 0x41, 0x81}, CodeOK, nil
}

// ---------------------------------------------------------------------------
// Set Session Privilege Level
// ---------------------------------------------------------------------------

func handleSetSessionPrivilegeLevel(_ context.Context, hctx *HandlerContext, req []byte) ([]byte, CompletionCode, error) {
	if len(req) < 1 {
		return nil, CodeRequestDataTruncated, nil
	}
	if hctx.Session == nil {
		return nil, CodeNotSupportedInState, nil
	}

	requested := bmc.PrivilegeLevel(req[0] & 0x0F)
	// Privilege 0 means "return current level" per spec.
	if requested == 0 {
		return []byte{uint8(hctx.Session.PrivilegeLevel)}, CodeOK, nil
	}
	if requested > hctx.Session.MaxPrivilege {
		return nil, CodeInsufficientPrivilege, nil
	}
	hctx.Session.PrivilegeLevel = requested
	return []byte{uint8(requested)}, CodeOK, nil
}

// ---------------------------------------------------------------------------
// Close Session
// ---------------------------------------------------------------------------

func handleCloseSession(_ context.Context, hctx *HandlerContext, req []byte) ([]byte, CompletionCode, error) {
	if len(req) < 4 {
		return nil, CodeRequestDataTruncated, nil
	}
	sessionID := binary.LittleEndian.Uint32(req[0:4])

	if err := hctx.BMC.Sessions.Close(sessionID); err != nil {
		return nil, CodeParamOutOfRange, nil
	}
	return nil, CodeOK, nil
}

// ---------------------------------------------------------------------------
// RMCP+ Open Session (payload type 0x10)
// ---------------------------------------------------------------------------

// OpenSessionRequest holds the fields from an RMCP+ Open Session Request message.
type OpenSessionRequest struct {
	MessageTag      uint8
	MaxPrivilege    uint8
	ConsoleID       uint32
	AuthAlgPayload  [8]byte
	IntAlgPayload   [8]byte
	CryptAlgPayload [8]byte
}

// OpenSessionResponse is the BMC's reply.
type OpenSessionResponse struct {
	MessageTag   uint8
	StatusCode   uint8
	MaxPrivilege uint8
	ConsoleID    uint32
	BMCID        uint32
	AuthAlg      uint8
	IntAlg       uint8
	CryptAlg     uint8
}

// HandleOpenSession processes an RMCP+ Open Session Request and returns the
// raw response payload.  It is called by the server before a session exists.
func HandleOpenSession(ctx context.Context, b *bmc.BMC, data []byte) ([]byte, error) {
	if len(data) < 32 {
		return buildOpenSessionError(0, 0, 0x12), nil // Illegal parameter
	}

	tag := data[0]
	maxPriv := data[1] & 0x0F
	consoleID := binary.LittleEndian.Uint32(data[4:8])

	// Parse algorithm payloads (3 x 8-byte records at offsets 8, 16, 24).
	authAlg := bmc.AuthAlg(data[12])     // byte 4 of auth payload
	intAlg := bmc.IntegrityAlg(data[20]) // byte 4 of integrity payload
	cryptAlg := bmc.CryptAlg(data[28])   // byte 4 of crypt payload

	// Validate algorithm support.  We support RAKP-HMAC-SHA1 (0x01),
	// HMAC-SHA1-96 (0x01), AES-CBC-128 (0x01) as the reference cipher suite.
	if authAlg != bmc.AuthAlgNone && authAlg != bmc.AuthAlgHMACSHA1 {
		return buildOpenSessionError(tag, consoleID, 0x04), nil // Invalid auth alg
	}
	if intAlg != bmc.IntegrityAlgNone && intAlg != bmc.IntegrityAlgHMACSHA1_96 {
		return buildOpenSessionError(tag, consoleID, 0x05), nil // Invalid integrity alg
	}
	if cryptAlg != bmc.CryptAlgNone && cryptAlg != bmc.CryptAlgAESCBC128 {
		return buildOpenSessionError(tag, consoleID, 0x10), nil // Invalid confidentiality alg
	}

	sess, err := b.Sessions.Allocate(consoleID, authAlg, intAlg, cryptAlg)
	if err != nil {
		return buildOpenSessionError(tag, consoleID, 0x01), nil // Insufficient resources
	}
	sess.MaxPrivilege = bmc.PrivilegeLevel(maxPriv)
	if sess.MaxPrivilege == 0 {
		sess.MaxPrivilege = bmc.PrivilegeLevelAdministrator
	}

	resp := make([]byte, 36)
	resp[0] = tag
	resp[1] = 0x00 // no error
	resp[2] = uint8(sess.MaxPrivilege)
	resp[3] = 0x00 // reserved
	binary.LittleEndian.PutUint32(resp[4:8], consoleID)
	binary.LittleEndian.PutUint32(resp[8:12], sess.BMCID)
	// Algorithm payloads (3 × 8 bytes).  resp is zero-initialised, so only
	// the non-zero fields need to be set.
	//   [PayloadType][reserved×2][0x08][Algorithm][reserved×3]
	resp[12] = 0x00 // auth
	resp[15] = 0x08 // payload length
	resp[16] = uint8(authAlg)
	resp[20] = 0x01 // integrity
	resp[23] = 0x08
	resp[24] = uint8(intAlg)
	resp[28] = 0x02 // confidentiality
	resp[31] = 0x08
	resp[32] = uint8(cryptAlg)

	return resp, nil
}

func buildOpenSessionError(tag uint8, consoleID uint32, statusCode uint8) []byte {
	resp := make([]byte, 8)
	resp[0] = tag
	resp[1] = statusCode
	resp[2] = 0x00
	resp[3] = 0x00
	binary.LittleEndian.PutUint32(resp[4:8], consoleID)
	return resp
}

// ---------------------------------------------------------------------------
// RAKP Message 1 → Message 2  (payload types 0x12, 0x13)
// ---------------------------------------------------------------------------

// HandleRAKP1 processes RAKP Message 1 and produces RAKP Message 2.
// It is called before the session is active; the session is identified by the
// BMC session ID embedded in Message 1.
func HandleRAKP1(ctx context.Context, b *bmc.BMC, data []byte) ([]byte, error) {
	if len(data) < 28 {
		return rakp2Error(0, 0, 0x12), nil
	}

	tag := data[0]
	bmcSessionID := binary.LittleEndian.Uint32(data[4:8])

	sess, err := b.Sessions.Get(bmcSessionID)
	if err != nil {
		return rakp2Error(tag, 0, 0x02), nil // Invalid Session ID
	}
	if sess.State != bmc.SessionStatePending {
		return rakp2Error(tag, sess.ConsoleID, 0x08), nil // Inactive Session ID
	}

	// Store the console's random number and requested role.
	copy(sess.ConsoleRand[:], data[8:24])
	sess.Role = data[24] // whole privilege byte including name-only bit

	usernameLen := data[27]
	if int(28+usernameLen) > len(data) {
		return rakp2Error(tag, sess.ConsoleID, 0x0C), nil // Invalid name length
	}
	username := string(data[28 : 28+usernameLen])

	// Look up user.
	user, lookupErr := b.Users.GetByName(username)
	if lookupErr != nil {
		// Spec says we must still generate a valid-looking response to prevent
		// user enumeration; we use a zero password for the HMAC then fail on RAKP3.
		user = nil
	}
	sess.User = user

	// Generate BMC random number.
	if _, err := rand.Read(sess.BMCRand[:]); err != nil {
		return rakp2Error(tag, sess.ConsoleID, 0xFF), fmt.Errorf("generate bmc rand: %w", err)
	}

	// Compute Key Exchange Authentication Code (HMAC over session params).
	authCode, err := computeRAKP2AuthCode(sess, b)
	if err != nil {
		return rakp2Error(tag, sess.ConsoleID, 0xFF), err
	}

	resp := make([]byte, 40+len(authCode))
	resp[0] = tag
	resp[1] = 0x00 // no error
	resp[2] = 0x00
	resp[3] = 0x00
	binary.LittleEndian.PutUint32(resp[4:8], sess.ConsoleID)
	copy(resp[8:24], sess.BMCRand[:])
	copy(resp[24:40], b.GUID[:])
	copy(resp[40:], authCode)
	return resp, nil
}

func rakp2Error(tag uint8, consoleID uint32, status uint8) []byte {
	resp := make([]byte, 8)
	resp[0] = tag
	resp[1] = status
	binary.LittleEndian.PutUint32(resp[4:8], consoleID)
	return resp
}

// ---------------------------------------------------------------------------
// RAKP Message 3 → Message 4  (payload types 0x14, 0x15)
// ---------------------------------------------------------------------------

// HandleRAKP3 processes RAKP Message 3, verifies the console's HMAC, derives
// session keys, marks the session active, and returns RAKP Message 4.
func HandleRAKP3(ctx context.Context, b *bmc.BMC, data []byte) ([]byte, error) {
	if len(data) < 8 {
		return rakp4Error(0, 0, 0x12), nil
	}

	tag := data[0]
	statusCode := data[1]
	bmcSessionID := binary.LittleEndian.Uint32(data[4:8])

	sess, err := b.Sessions.Get(bmcSessionID)
	if err != nil {
		return rakp4Error(tag, 0, 0x02), nil // Invalid Session ID
	}

	// If the console sent a non-zero status in RAKP3, it means the console
	// rejected RAKP2.  Close the session and return an error response.
	if statusCode != 0x00 {
		_ = b.Sessions.Close(bmcSessionID)
		return rakp4Error(tag, sess.ConsoleID, statusCode), nil
	}

	// Verify the auth code sent by the console.
	authCodeLen := rakp3AuthCodeLen(sess.AuthAlg)
	if len(data) < 8+authCodeLen {
		return rakp4Error(tag, sess.ConsoleID, 0x0F), nil // Invalid integrity check value
	}
	consoleAuthCode := data[8 : 8+authCodeLen]

	expected, err := computeRAKP3AuthCode(sess, b)
	if err != nil {
		return rakp4Error(tag, sess.ConsoleID, 0xFF), err
	}

	if sess.User == nil || !hmacEqual(expected, consoleAuthCode) {
		_ = b.Sessions.Close(bmcSessionID)
		return rakp4Error(tag, sess.ConsoleID, 0x0D), nil // Unauthorized name
	}

	// Derive SIK, K1, K2.
	if err := deriveSessKeys(sess, b); err != nil {
		return rakp4Error(tag, sess.ConsoleID, 0xFF), err
	}
	sess.State = bmc.SessionStateActive
	sess.PrivilegeLevel = sess.MaxPrivilege

	// Compute RAKP4 auth code using SIK as HMAC key.
	rakp4Code, err := computeRAKP4AuthCode(sess, b)
	if err != nil {
		return rakp4Error(tag, sess.ConsoleID, 0xFF), err
	}

	resp := make([]byte, 8+len(rakp4Code))
	resp[0] = tag
	resp[1] = 0x00
	resp[2] = 0x00
	resp[3] = 0x00
	binary.LittleEndian.PutUint32(resp[4:8], sess.ConsoleID)
	copy(resp[8:], rakp4Code)
	return resp, nil
}

func rakp4Error(tag uint8, consoleID uint32, status uint8) []byte {
	resp := make([]byte, 8)
	resp[0] = tag
	resp[1] = status
	binary.LittleEndian.PutUint32(resp[4:8], consoleID)
	return resp
}
