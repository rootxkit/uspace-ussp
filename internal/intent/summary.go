package intent

import (
	"context"
	"errors"
	"slices"
	"time"
)

// MaxSummaryRows bounds each count of a CountSummary: at most this many
// open intents, and the closed states among at most this many newest
// intents, are read.
const MaxSummaryRows = 10000

// ClosedStates are the states an intent ends its life in.
var ClosedStates = []string{StateEnded, StateRejected, StateWithdrawn}

// States are every local state, open first.
var States = append(slices.Clone(OpenStates), ClosedStates...)

// StateCounts are the store's counts of the intents by state.
type StateCounts struct {
	// AsOf is the database clock when they were counted.
	AsOf time.Time
	// Open are the open intents by state, at most the bound read.
	Open map[string]int
	// Newest are the newest intents by state, every state, at most
	// the bound read.
	Newest map[string]int
}

// CountSummary is GET /v1/intents/summary (an operator's intents) and GET
// /v1/admin/intents/summary (every operator's): the intents counted by
// state on the database clock, no intent and no person named. The open
// states count every open intent; the closed states count those among
// the newest MaxSummaryRows intents. Truncated says a count reached its
// bound: the counts and the total are then lower bounds.
type CountSummary struct {
	AsOf      time.Time      `json:"as_of"`
	Counts    map[string]int `json:"counts"`
	Total     int            `json:"total"`
	Truncated bool           `json:"truncated"`
}

// SummaryOperator counts the operator's intents (a portal session's).
// An empty operator is refused, never answered with every operator's.
func (s *Service) SummaryOperator(ctx context.Context, operatorID string) (CountSummary, error) {
	if operatorID == "" {
		return CountSummary{}, errors.New("intent summary: no operator")
	}
	return s.summary(ctx, &operatorID)
}

// SummaryAll counts every operator's intents (the console's).
func (s *Service) SummaryAll(ctx context.Context) (CountSummary, error) {
	return s.summary(ctx, nil)
}

func (s *Service) summary(ctx context.Context, operatorID *string) (CountSummary, error) {
	c, err := s.Store.StateCounts(ctx, operatorID, MaxSummaryRows+1)
	if err != nil {
		return CountSummary{}, &UnavailableError{Dependency: "database", Detail: "the intents could not be counted"}
	}
	out := CountSummary{AsOf: c.AsOf.UTC(), Counts: make(map[string]int, len(States))}
	open, newest := 0, 0
	for _, st := range States {
		out.Counts[st] = 0
	}
	for st, n := range c.Open {
		if slices.Contains(OpenStates, st) {
			out.Counts[st] = n
		}
		open += n
	}
	for st, n := range c.Newest {
		if slices.Contains(ClosedStates, st) {
			out.Counts[st] = n
		}
		newest += n
	}
	out.Truncated = open > MaxSummaryRows || newest > MaxSummaryRows
	for _, n := range out.Counts {
		out.Total += n
	}
	return out, nil
}
