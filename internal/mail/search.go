package mail

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/api/gmail/v1"
)

// ListOptions controls a search/list call. Query is native Gmail search
// syntax (from:, is:unread, newer_than:7d, ...) — passed straight through to
// the API's q= parameter, no translation needed since this hits the REST
// API directly rather than IMAP SEARCH. See docket-design.md §4.
type ListOptions struct {
	Query    string
	LabelIDs []string
	Limit    int64
}

const defaultLimit = 25
const maxLimit = 500

// concurrentFetches bounds how many Messages.Get calls run at once when
// hydrating envelopes for a page of list/search results.
const concurrentFetches = 8

// List runs a search or label listing and returns lightweight envelopes.
// It never returns bodies — see Envelope's doc comment.
func List(ctx context.Context, svc *gmail.Service, labels *LabelCache, opts ListOptions) ([]Envelope, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		return nil, fmt.Errorf("--limit %d exceeds the maximum of %d", limit, maxLimit)
	}

	call := svc.Users.Messages.List(meUser).MaxResults(limit).Context(ctx)
	if opts.Query != "" {
		call = call.Q(opts.Query)
	}
	if len(opts.LabelIDs) > 0 {
		call = call.LabelIds(opts.LabelIDs...)
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

	return envelopes, nil
}
