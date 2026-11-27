package gateway

import (
	"sort"
	"time"
)

// Model is one entry of the listing.
type Model struct {
	ID      string
	Created time.Time
}

// Catalog is an immutable view of the gateway config at one point in time.
type Catalog struct {
	Routes  []Route
	KeyAuth KeyAuth
}

// Models returns the models consumer may call through the gateway when it
// sends requests to host, sorted by id. A model served by several routes is
// listed when any of them lets the consumer through; Created is the earliest
// such route's creation time.
func (c *Catalog) Models(consumer, host string) []Model {
	if c == nil || consumer == "" {
		return nil
	}
	host = StripPort(host)
	byID := map[string]time.Time{}
	for _, r := range c.Routes {
		if !r.ServesHost(host) {
			continue
		}
		allowed, ok := c.KeyAuth.Allowed(r.Name, host)
		if !ok {
			continue
		}
		if _, in := allowed[consumer]; !in {
			continue
		}
		if t, seen := byID[r.Model]; !seen || r.Created.Before(t) {
			byID[r.Model] = r.Created
		}
	}
	out := make([]Model, 0, len(byID))
	for id, t := range byID {
		out = append(out, Model{ID: id, Created: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// OpenRoute is a model route key-auth does not authenticate.
type OpenRoute struct {
	Route, Model string
	// Where says for which requests: "on host X", or, for a route without
	// hosts, "for hosts no key-auth domain rule matches".
	Where string
}

// OpenModelRoutes lists model routes key-auth does not authenticate, for
// logging the misconfiguration. Their models are listed for nobody there. A
// route declaring hosts is checked per host. A route without hosts is checked
// for a host that no domain rule matches; requests on hosts a domain rule
// covers are decided per request.
func (c *Catalog) OpenModelRoutes() []OpenRoute {
	var out []OpenRoute
	for _, r := range c.Routes {
		if len(r.Hosts) == 0 {
			if _, ok := c.KeyAuth.Allowed(r.Name, ""); !ok {
				out = append(out, OpenRoute{r.Name, r.Model, "for hosts no key-auth domain rule matches"})
			}
			continue
		}
		for _, h := range r.Hosts {
			if _, ok := c.KeyAuth.Allowed(r.Name, h); !ok {
				out = append(out, OpenRoute{r.Name, r.Model, "on host " + h})
			}
		}
	}
	return out
}
