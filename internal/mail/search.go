package mail

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/api/gmail/v1"
)

// ListOptions controls a search/list call. Query is native Gmail search
// syntax (from:, is:unread, newer_than:7d, ...) — passed straight through to
// the API's q= parameter, no translation needed since this hits the REST
// API directly rather than IMAP SEARCH. See docket-design.md §4.
//
// PageToken continues an earlier call: pass back the NextPageToken that call
// returned. Limit is the size of one page, not a budget spread across pages,
// so the two compose — the same Limit applies to every page of a walk.
type ListOptions struct {
	Query     string
	LabelIDs  []string
	Limit     int64
	PageToken string
}

// DefaultLimit and MaxLimit bound one page. MaxLimit is Gmail's own
// maxResults ceiling for messages.list; a caller wanting more than that walks
// pages rather than asking for a bigger one, which is why exceeding it is a
// usage error and not something docket silently clamps. Clamping would hand
// back 500 of 3000 results looking exactly like a complete answer.
const (
	DefaultLimit = 25
	MaxLimit     = 500
)

// ErrLimitTooLarge marks an out-of-range Limit as the caller's mistake.
// Retrying the identical call cannot succeed, so it must never be reported to
// a client as retryable.
var ErrLimitTooLarge = errors.New("limit exceeds the maximum")

// ListResult is one page of search/list output. NextPageToken is empty when
// this page is the last: that is the only signal Gmail gives, and it is what
// tells a caller whether it is holding the whole result set.
type ListResult struct {
	Envelopes     []Envelope
	NextPageToken string
}

// concurrentFetches bounds how many Messages.Get calls run at once when
// hydrating envelopes for a page of list/search results.
const concurrentFetches = 8

// List runs a search or label listing and returns one page of lightweight
// envelopes. It never returns bodies — see Envelope's doc comment.
func List(ctx context.Context, svc *gmail.Service, labels *LabelCache, opts ListOptions) (*ListResult, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		return nil, fmt.Errorf("--limit %d exceeds the maximum of %d: %w", limit, MaxLimit, ErrLimitTooLarge)
	}

	call := svc.Users.Messages.List(meUser).MaxResults(limit).Context(ctx)
	if opts.Query != "" {
		call = call.Q(opts.Query)
	}
	if len(opts.LabelIDs) > 0 {
		call = call.LabelIds(opts.LabelIDs...)
	}
	if opts.PageToken != "" {
		call = call.PageToken(opts.PageToken)
	}

	resp, err := call.Do()
	if err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}

	envelopes := make([]Envelope, len(resp.Messages))
	errs := make([]error, len(resp.Messages))

	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrentFetches)
	for i, stub := range resp.Messages {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			msg, err := svc.Users.Messages.Get(meUser, id).
				Format("metadata").
				MetadataHeaders(metadataHeaders...).
				Context(ctx).Do()
			if err != nil {
				errs[i] = fmt.Errorf("fetching message %s: %w", id, err)
				return
			}
			envelopes[i] = envelopeFromMessage(msg, labels)
		}(i, stub.Id)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	return &ListResult{Envelopes: envelopes, NextPageToken: resp.NextPageToken}, nil
}
