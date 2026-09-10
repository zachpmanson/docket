package mail

import (
	"context"
	"fmt"

	"google.golang.org/api/gmail/v1"
)

// LabelCache maps Gmail label ids (e.g. "Label_12", "INBOX") to their
// display names and back, fetched once per invocation. Custom labels have
// opaque ids; system labels (INBOX, UNREAD, ...) use their name as the id.
type LabelCache struct {
	idToName map[string]string
	nameToID map[string]string
}

// LoadLabels fetches the full label list for the authenticated user.
func LoadLabels(ctx context.Context, svc *gmail.Service) (*LabelCache, error) {
	resp, err := svc.Users.Labels.List(meUser).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("listing labels: %w", err)
	}
	c := &LabelCache{
		idToName: make(map[string]string, len(resp.Labels)),
		nameToID: make(map[string]string, len(resp.Labels)),
	}
	for _, l := range resp.Labels {
		c.idToName[l.Id] = l.Name
		c.nameToID[l.Name] = l.Id
	}
	return c, nil
}

// Names translates label ids to display names, passing through unknown ids
// unchanged rather than dropping them.
func (c *LabelCache) Names(ids []string) []string {
	names := make([]string, len(ids))
	for i, id := range ids {
		if name, ok := c.idToName[id]; ok {
			names[i] = name
		} else {
			names[i] = id
		}
	}
	return names
}

// ID resolves a label display name to its id. Returns ok=false if no such
// label exists, so the caller can produce an LLM-friendly error listing the
// labels that do exist.
func (c *LabelCache) ID(name string) (id string, ok bool) {
	id, ok = c.nameToID[name]
	return id, ok
}

// AllNames returns every known label name, for error messages that need to
// suggest valid alternatives.
func (c *LabelCache) AllNames() []string {
	names := make([]string, 0, len(c.nameToID))
	for name := range c.nameToID {
		names = append(names, name)
	}
	return names
}
