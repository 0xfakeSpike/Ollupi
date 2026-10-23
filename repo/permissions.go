package repo

// PermissionRules is the persistence shape of a permission rule set. The
// agent/permission package parses these strings and makes the decisions;
// repo must not import agent/permission, so the neutral shape lives here.
type PermissionRules struct {
	Allow []string `json:"allow,omitempty"`
	Ask   []string `json:"ask,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

func (r PermissionRules) Set() bool {
	return len(r.Allow) > 0 || len(r.Ask) > 0 || len(r.Deny) > 0
}
