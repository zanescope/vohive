package mbim

import (
	"context"
	"testing"
)

func TestQueryConnectUsesFixedQueryStructureAndParsesState(t *testing.T) {
	var request []byte
	tr := NewFakeTransport(func(written []byte) ([]byte, bool) {
		h, err := decodeHeader(written)
		if err != nil {
			return nil, false
		}
		if h.Type == MessageTypeOpen {
			return buildOpenDone(h.TransactionID), true
		}
		if h.Type != MessageTypeCommand || len(written) < 48 {
			return nil, false
		}
		var service UUID
		copy(service[:], written[20:36])
		cid := le.Uint32(written[36:])
		commandType := le.Uint32(written[40:])
		if service.Equal(UUIDBasicConnect) && cid == CIDBasicConnectConnect &&
			commandType == uint32(CommandTypeQuery) {
			request = append([]byte(nil), written[48:]...)
			info := make([]byte, 36)
			le.PutUint32(info[0:], 7)
			le.PutUint32(info[4:], ActivationStateActivated)
			le.PutUint32(info[12:], ContextIPTypeIPv4v6)
			copy(info[16:32], UUIDContextTypeInternet[:])
			le.PutUint32(info[32:], 33)
			return buildCommandDone(h.TransactionID, service, cid, info), true
		}
		return buildCommandDone(h.TransactionID, service, cid, nil), true
	})

	device := NewDevice(tr)
	if err := device.Open(context.Background(), 4096); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer device.Close()

	state, err := QueryConnect(context.Background(), device, 7)
	if err != nil {
		t.Fatalf("QueryConnect: %v", err)
	}
	if len(request) != 36 || le.Uint32(request) != 7 {
		t.Fatalf("CONNECT query request = %x, want 36-byte structure with SessionId 7", request)
	}
	for i, value := range request[4:] {
		if value != 0 {
			t.Fatalf("CONNECT query reserved byte %d = %#x, want zero", i+4, value)
		}
	}
	if state.SessionID != 7 || state.ActivationState != ActivationStateActivated ||
		state.IPType != ContextIPTypeIPv4v6 || state.NwError != 33 {
		t.Fatalf("CONNECT query state = %+v", state)
	}
}

func TestQueryIPConfigurationUsesFixedQueryStructure(t *testing.T) {
	var request []byte
	tr := NewFakeTransport(func(written []byte) ([]byte, bool) {
		h, err := decodeHeader(written)
		if err != nil {
			return nil, false
		}
		if h.Type == MessageTypeOpen {
			return buildOpenDone(h.TransactionID), true
		}
		if h.Type != MessageTypeCommand || len(written) < 48 {
			return nil, false
		}
		var service UUID
		copy(service[:], written[20:36])
		cid := le.Uint32(written[36:])
		commandType := le.Uint32(written[40:])
		if service.Equal(UUIDBasicConnect) && cid == CIDBasicConnectIPConfiguration &&
			commandType == uint32(CommandTypeQuery) {
			request = append([]byte(nil), written[48:]...)
			return buildCommandDone(h.TransactionID, service, cid, make([]byte, 60)), true
		}
		return buildCommandDone(h.TransactionID, service, cid, nil), true
	})

	device := NewDevice(tr)
	if err := device.Open(context.Background(), 4096); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer device.Close()

	if _, err := QueryIPConfiguration(context.Background(), device, 9); err != nil {
		t.Fatalf("QueryIPConfiguration: %v", err)
	}
	if len(request) != 60 || le.Uint32(request) != 9 {
		t.Fatalf("IP_CONFIGURATION query len/session = %d/%d, want 60/9", len(request), le.Uint32(request))
	}
	for i, value := range request[4:] {
		if value != 0 {
			t.Fatalf("IP_CONFIGURATION query byte %d = 0x%02x, want zero", i+4, value)
		}
	}
}
