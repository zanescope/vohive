package qmicore

import (
	"context"
	"errors"
	"testing"

	"github.com/zanescope/quectel-qmi-go/pkg/qmi"
)

type rawSMSAckSenderStub struct {
	request qmi.WMSAckRequest
	result  *qmi.WMSAckResult
	err     error
}

func (s *rawSMSAckSenderStub) WMSSendAck(_ context.Context, req qmi.WMSAckRequest) (*qmi.WMSAckResult, error) {
	s.request = req
	return s.result, s.err
}

func TestSendRawSMSAckPreservesResultWhenErrorReturned(t *testing.T) {
	wantResult := &qmi.WMSAckResult{HasFailureCause: true, FailureCause: 2}
	wantErr := errors.New("send ack failed")
	sender := &rawSMSAckSenderStub{result: wantResult, err: wantErr}

	gotResult, gotErr := sendRawSMSAck(context.Background(), sender, RawSMSIndication{
		TransactionID: ^uint32(0),
		Format:        0x06,
	}, true)

	if gotResult != wantResult {
		t.Fatalf("result=%p want %p", gotResult, wantResult)
	}
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("err=%v want %v", gotErr, wantErr)
	}
	if sender.request.TransactionID != ^uint32(0) {
		t.Fatalf("transaction=0x%x want 0xffffffff", sender.request.TransactionID)
	}
	if sender.request.Protocol != qmi.WMSMessageProtocolWCDMA {
		t.Fatalf("protocol=%v want WCDMA", sender.request.Protocol)
	}
	if !sender.request.Success {
		t.Fatal("success=false want true")
	}
}

func TestSendRawSMSAckUsesCDMAProtocolForCDMAFormat(t *testing.T) {
	sender := &rawSMSAckSenderStub{result: &qmi.WMSAckResult{}}

	result, err := sendRawSMSAck(context.Background(), sender, RawSMSIndication{
		TransactionID: 7,
		Format:        0x00,
	}, true)

	if err != nil {
		t.Fatalf("sendRawSMSAck error: %v", err)
	}
	if result != sender.result {
		t.Fatalf("result=%p want %p", result, sender.result)
	}
	if sender.request.Protocol != qmi.WMSMessageProtocolCDMA {
		t.Fatalf("protocol=%v want CDMA", sender.request.Protocol)
	}
}
