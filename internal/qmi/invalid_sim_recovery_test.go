package qmicore

import (
	"errors"
	"fmt"
	"testing"
	"time"

	qmimanager "github.com/zanescope/quectel-qmi-go/pkg/manager"
	"github.com/zanescope/quectel-qmi-go/pkg/qmi"
)

func invalidSIMStateDialError() error {
	return fmt.Errorf("IPv4 dial failed: %w", &qmi.StartNetworkError{
		Err: errors.New("QMI call failed"),
		Reason: &qmi.CallEndReason{
			Type: qmiCallEndReasonTypeCallManager,
			Code: qmiCallEndReasonCMInvalidSIMState,
		},
	})
}

func TestQMIErrorIsInvalidSIMStateUsesStructuredReason(t *testing.T) {
	if !qmiErrorIsInvalidSIMState(invalidSIMStateDialError()) {
		t.Fatal("structured CM_INVALID_SIM_STATE error was not recognized")
	}

	cases := []error{
		nil,
		errors.New("call end type=3 code=2504"),
		&qmi.StartNetworkError{Reason: &qmi.CallEndReason{Type: 2, Code: qmiCallEndReasonCMInvalidSIMState}},
		&qmi.StartNetworkError{Reason: &qmi.CallEndReason{Type: qmiCallEndReasonTypeCallManager, Code: 2503}},
	}
	for _, err := range cases {
		if qmiErrorIsInvalidSIMState(err) {
			t.Fatalf("qmiErrorIsInvalidSIMState(%v) = true, want false", err)
		}
	}
}

func TestInvalidSIMDialFailureStateEscalatesOnceAtThreshold(t *testing.T) {
	var state invalidSIMDialFailureState
	now := time.Date(2026, 8, 19, 22, 10, 55, 0, time.UTC)
	err := invalidSIMStateDialError()

	for attempt := 1; attempt < qmiInvalidSIMDialFailureThreshold; attempt++ {
		count, escalate := state.observe(7, err, now.Add(time.Duration(attempt)*10*time.Second))
		if count != attempt || escalate {
			t.Fatalf("attempt %d: count=%d escalate=%t", attempt, count, escalate)
		}
	}
	count, escalate := state.observe(7, err, now.Add(time.Minute))
	if count != qmiInvalidSIMDialFailureThreshold || !escalate {
		t.Fatalf("threshold attempt: count=%d escalate=%t", count, escalate)
	}
	if _, escalate := state.observe(7, err, now.Add(70*time.Second)); escalate {
		t.Fatal("state escalated more than once for the same failure streak")
	}
}

func TestInvalidSIMDialFailureStateResetsAcrossBoundaries(t *testing.T) {
	now := time.Date(2026, 8, 19, 22, 10, 55, 0, time.UTC)
	err := invalidSIMStateDialError()

	var state invalidSIMDialFailureState
	state.observe(1, err, now)
	if count, _ := state.observe(2, err, now.Add(time.Second)); count != 1 {
		t.Fatalf("generation change count=%d, want 1", count)
	}
	if count, _ := state.observe(2, err, now.Add(qmiInvalidSIMDialFailureWindow+2*time.Second)); count != 1 {
		t.Fatalf("window expiry count=%d, want 1", count)
	}
	if count, _ := state.observe(2, errors.New("another dial failure"), now.Add(qmiInvalidSIMDialFailureWindow+3*time.Second)); count != 0 {
		t.Fatalf("different error count=%d, want 0", count)
	}
}

func TestManagerPersistentInvalidSIMStateDispatchesRecovery(t *testing.T) {
	m := &Manager{}
	type recovery struct {
		reason string
		err    error
	}
	got := make(chan recovery, 2)
	m.OnRecoveryExhausted(func(reason string, err error) {
		got <- recovery{reason: reason, err: err}
	})

	event := qmimanager.Event{
		Type:       qmimanager.EventDialFailed,
		Generation: 9,
		Error:      invalidSIMStateDialError(),
	}
	for attempt := 0; attempt < qmiInvalidSIMDialFailureThreshold; attempt++ {
		m.handleQMIEvent(event)
	}

	select {
	case result := <-got:
		if result.reason != qmiInvalidSIMRecoveryReason {
			t.Fatalf("reason=%q, want %q", result.reason, qmiInvalidSIMRecoveryReason)
		}
		if !qmiErrorIsInvalidSIMState(result.err) {
			t.Fatalf("recovery error=%v, want structured invalid SIM error", result.err)
		}
	default:
		t.Fatal("persistent invalid SIM state did not dispatch recovery")
	}

	m.handleQMIEvent(event)
	select {
	case duplicate := <-got:
		t.Fatalf("duplicate recovery dispatch: %+v", duplicate)
	default:
	}

	m.handleQMIEvent(qmimanager.Event{Type: qmimanager.EventConnected, Generation: 9})
	for attempt := 0; attempt < qmiInvalidSIMDialFailureThreshold; attempt++ {
		m.handleQMIEvent(event)
	}
	select {
	case result := <-got:
		if result.reason != qmiInvalidSIMRecoveryReason {
			t.Fatalf("reason after connected reset=%q", result.reason)
		}
	default:
		t.Fatal("connected event did not reset invalid SIM recovery state")
	}
}
