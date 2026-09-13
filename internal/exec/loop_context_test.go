package exec

import (
	"testing"

	"github.com/michiTrader/arxi/internal/kernel"
)

// TestContextEventsAreRunLoopBookkeeping pins the loop-side classification of
// the context.* records. The run loop executes effects for domain events only:
// if a context record were treated as a domain step, bookkeeping would be
// executed as work, billed as a turn and waited on as quiescence.
func TestContextEventsAreRunLoopBookkeeping(t *testing.T) {
	for _, event := range []kernel.EventType{
		kernel.ContextPrepareRequested, kernel.ContextPrepared, kernel.ContextPrepareFailed,
		kernel.ExecWorkPrepared, kernel.ExecWorkStarted, kernel.ExecWorkFinished, kernel.ExecStepCompleted,
	} {
		if !isProgressEvent(event) {
			t.Fatalf("%s is not classified as progress: the run loop would execute coordination bookkeeping as if it were a domain step", event)
		}
	}
	for _, event := range []kernel.EventType{
		kernel.RunStarted, kernel.RunPrompt, kernel.AgentTurnDone, kernel.LLMResponse, kernel.ToolCall,
	} {
		if isProgressEvent(event) {
			t.Fatalf("%s is classified as progress: domain events must be decided and executed, never skipped as bookkeeping", event)
		}
	}
}
