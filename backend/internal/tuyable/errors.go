package tuyable

import (
	"errors"
	"fmt"
)

// Session errors. Edge maps them to the availability reasons it reports for a device.
var (
	// ErrKeyRejected: the device did not accept our keys. Either its device-info answer does not decrypt and
	// pass its CRC with the login key derived from local_key (and sec_key), or it answered the pair request
	// with a result other than 0 (paired) or 2 (already paired). Usually the device was re-paired in the Tuya
	// app, which rotates local_key, and the keys must be imported again.
	ErrKeyRejected = errors.New("tuyable: device rejected the keys")
	// ErrBusy: the device refused the connection. A Tuya BLE device accepts one central at a time, so this
	// usually means the phone app, a Tuya hub or another controller is connected to it. Connecting is the
	// radio's job: radios (and tuyablesim) return ErrBusy from their connect call; a Session never does.
	ErrBusy = errors.New("tuyable: device is connected to another central")
	// ErrTimeout: no answer within Config.ResponseTimeout. It wraps context.DeadlineExceeded; when the caller's
	// own context ends first, the caller's ctx.Err() is returned instead, without ErrTimeout.
	ErrTimeout = errors.New("tuyable: device did not answer in time")
	// ErrUnsupportedProtocol: the device speaks a protocol version this package does not implement, or the
	// operation needs a newer one (DP writes need protocol 3 or later).
	ErrUnsupportedProtocol = errors.New("tuyable: unsupported protocol version")
	// ErrClosed: the session was closed, or the link dropped.
	ErrClosed = errors.New("tuyable: session closed")
	// ErrFormat: a frame, fragment, advertisement or data point is malformed.
	ErrFormat = errors.New("tuyable: malformed data")
	// ErrCRC: a frame decrypted but its CRC16 does not match. With a wrong key this is the usual symptom.
	ErrCRC = errors.New("tuyable: frame CRC mismatch")
)

// DeviceError is a non-zero result code the device returned for a request.
type DeviceError struct {
	Code   Code
	Result byte
}

func (e *DeviceError) Error() string {
	return fmt.Sprintf("tuyable: device answered %s with result %d", e.Code, e.Result)
}
