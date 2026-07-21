package mbim

import (
	"context"
	"testing"
)

func TestEncodeSubscribeList(t *testing.T) {
	entries := []EventEntry{
		{Service: UUIDBasicConnect, CIDs: []uint32{CIDBasicConnectSignalState, CIDBasicConnectRegisterState}},
		{Service: UUIDSMS, CIDs: []uint32{CIDSMSRead}},
	}
	info := encodeSubscribeList(entries)
	if le.Uint32(info[0:]) != 2 {
		t.Fatalf("EventsCount = %d, want 2", le.Uint32(info[0:]))
	}
	off0 := le.Uint32(info[4:])
	if !bytesEqualUUID(info[off0:off0+16], UUIDBasicConnect) {
		t.Fatal("entry0 UUID mismatch")
	}
	if le.Uint32(info[int(off0)+16:]) != 2 {
		t.Fatalf("entry0 CidsCount = %d, want 2", le.Uint32(info[int(off0)+16:]))
	}
	if le.Uint32(info[int(off0)+20:]) != CIDBasicConnectSignalState {
		t.Fatal("entry0 first CID mismatch")
	}
}

func bytesEqualUUID(b []byte, u UUID) bool {
	if len(b) < 16 {
		return false
	}
	for i := 0; i < 16; i++ {
		if b[i] != u[i] {
			return false
		}
	}
	return true
}

func TestSubscribeDefaultEvents(t *testing.T) {
	ft := newFakeTransport()
	var subscribeInfo []byte
	ft.reply = func(w []byte) ([]byte, bool) {
		h, _ := decodeHeader(w)
		switch h.Type {
		case MessageTypeOpen:
			return openDoneMsg(h.TransactionID), true
		case MessageTypeCommand:
			if le.Uint32(w[36:]) == CIDBasicConnectDeviceServiceSubscribeList {
				subscribeInfo = append([]byte(nil), w[48:]...)
			}
			return makeCommandDoneFragmentFor(h.TransactionID, UUIDBasicConnect, le.Uint32(w[36:]), nil), true
		}
		return nil, false
	}
	d := newDevice(ft)
	if err := d.Open(context.Background(), 4096); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if err := SubscribeDefaultEvents(context.Background(), d); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(subscribeInfo) == 0 {
		t.Fatal("SUBSCRIBE_LIST command not sent")
	}

	var basicCIDs map[uint32]bool
	for i, count := 0, int(le.Uint32(subscribeInfo)); i < count; i++ {
		ref := 4 + i*8
		off := int(le.Uint32(subscribeInfo[ref:]))
		size := int(le.Uint32(subscribeInfo[ref+4:]))
		if off < 0 || size < 20 || off+size > len(subscribeInfo) {
			t.Fatalf("invalid subscription entry ref off=%d size=%d", off, size)
		}
		entry := subscribeInfo[off : off+size]
		if !bytesEqualUUID(entry[:16], UUIDBasicConnect) {
			continue
		}
		basicCIDs = make(map[uint32]bool)
		for j, cidCount := 0, int(le.Uint32(entry[16:])); j < cidCount; j++ {
			basicCIDs[le.Uint32(entry[20+j*4:])] = true
		}
	}
	if !basicCIDs[CIDBasicConnectConnect] {
		t.Fatal("default subscription omits CONNECT")
	}
	if !basicCIDs[CIDBasicConnectIPConfiguration] {
		t.Fatal("default subscription omits IP_CONFIGURATION")
	}
}
func TestSubscribeDefaultEventsFallsBackToConnectOnlyList(t *testing.T) {
	ft := newFakeTransport()
	var subscribeInfos [][]byte
	ft.reply = func(w []byte) ([]byte, bool) {
		h, _ := decodeHeader(w)
		switch h.Type {
		case MessageTypeOpen:
			return openDoneMsg(h.TransactionID), true
		case MessageTypeCommand:
			cid := le.Uint32(w[36:])
			if cid != CIDBasicConnectDeviceServiceSubscribeList {
				return makeCommandDoneFragmentFor(h.TransactionID, UUIDBasicConnect, cid, nil), true
			}
			subscribeInfos = append(subscribeInfos, append([]byte(nil), w[48:]...))
			response := makeCommandDoneFragmentFor(h.TransactionID, UUIDBasicConnect, cid, nil)
			if len(subscribeInfos) == 1 {
				le.PutUint32(response[40:], 1)
			}
			return response, true
		}
		return nil, false
	}

	d := newDevice(ft)
	if err := d.Open(context.Background(), 4096); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if err := SubscribeDefaultEvents(context.Background(), d); err != nil {
		t.Fatalf("SubscribeDefaultEvents fallback: %v", err)
	}
	if len(subscribeInfos) != 2 {
		t.Fatalf("SUBSCRIBE_LIST attempts = %d, want extended + CONNECT-only fallback", len(subscribeInfos))
	}

	extended := subscribedBasicCIDs(t, subscribeInfos[0])
	connectOnly := subscribedBasicCIDs(t, subscribeInfos[1])
	if !extended[CIDBasicConnectConnect] || !extended[CIDBasicConnectIPConfiguration] {
		t.Fatalf("extended subscription missing data CIDs: %v", extended)
	}
	if !connectOnly[CIDBasicConnectConnect] || connectOnly[CIDBasicConnectIPConfiguration] {
		t.Fatalf("CONNECT-only fallback has wrong data CIDs: %v", connectOnly)
	}
	for _, cid := range []uint32{
		CIDBasicConnectSignalState,
		CIDBasicConnectRegisterState,
		CIDBasicConnectPacketService,
		CIDBasicConnectSubscriberReadyStatus,
	} {
		if !connectOnly[cid] {
			t.Fatalf("CONNECT-only fallback missing CID %d: %v", cid, connectOnly)
		}
	}
}

func TestSubscribeDefaultEventsFallsBackToLegacyList(t *testing.T) {
	ft := newFakeTransport()
	var subscribeInfos [][]byte
	ft.reply = func(w []byte) ([]byte, bool) {
		h, _ := decodeHeader(w)
		switch h.Type {
		case MessageTypeOpen:
			return openDoneMsg(h.TransactionID), true
		case MessageTypeCommand:
			cid := le.Uint32(w[36:])
			response := makeCommandDoneFragmentFor(h.TransactionID, UUIDBasicConnect, cid, nil)
			if cid == CIDBasicConnectDeviceServiceSubscribeList {
				subscribeInfos = append(subscribeInfos, append([]byte(nil), w[48:]...))
				if len(subscribeInfos) <= 2 {
					le.PutUint32(response[40:], 1)
				}
			}
			return response, true
		}
		return nil, false
	}

	d := newDevice(ft)
	if err := d.Open(context.Background(), 4096); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if err := SubscribeDefaultEvents(context.Background(), d); err != nil {
		t.Fatalf("SubscribeDefaultEvents legacy fallback: %v", err)
	}
	if len(subscribeInfos) != 3 {
		t.Fatalf("SUBSCRIBE_LIST attempts = %d, want extended + CONNECT-only + legacy", len(subscribeInfos))
	}
	connectOnly := subscribedBasicCIDs(t, subscribeInfos[1])
	legacy := subscribedBasicCIDs(t, subscribeInfos[2])
	if !connectOnly[CIDBasicConnectConnect] || connectOnly[CIDBasicConnectIPConfiguration] {
		t.Fatalf("second attempt is not CONNECT-only: %v", connectOnly)
	}
	if legacy[CIDBasicConnectConnect] || legacy[CIDBasicConnectIPConfiguration] {
		t.Fatalf("legacy fallback retained data CIDs: %v", legacy)
	}
}

func subscribedBasicCIDs(t *testing.T, info []byte) map[uint32]bool {
	t.Helper()
	for i, count := 0, int(le.Uint32(info)); i < count; i++ {
		ref := 4 + i*8
		if ref+8 > len(info) {
			t.Fatalf("subscription reference %d is truncated", i)
		}
		off := int(le.Uint32(info[ref:]))
		size := int(le.Uint32(info[ref+4:]))
		if off < 0 || size < 20 || off+size > len(info) {
			t.Fatalf("invalid subscription entry ref off=%d size=%d", off, size)
		}
		entry := info[off : off+size]
		if !bytesEqualUUID(entry[:16], UUIDBasicConnect) {
			continue
		}
		cids := make(map[uint32]bool)
		for j, count := 0, int(le.Uint32(entry[16:])); j < count; j++ {
			cids[le.Uint32(entry[20+j*4:])] = true
		}
		return cids
	}
	t.Fatal("subscription has no Basic Connect entry")
	return nil
}
