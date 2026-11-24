package app

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// AgentTokenOpts selects what `webwatcher agenttoken` does.
type AgentTokenOpts struct {
	Revoke bool // revoke every active agent token of the account
	Status bool // list the tokens (never the secret)
}

// AgentToken creates a Home Agent token for a number (creating the account
// when absent), lists the account's tokens, or revokes them all. The token is
// printed once and only its hash is stored.
func AgentToken(getenv func(string) string, phone string, o AgentTokenOpts, w io.Writer) error {
	if o.Revoke && o.Status {
		return errors.New("use only one of --revoke and --show-status")
	}
	c, err := openCLI(getenv, "agenttoken", phone, !o.Revoke && !o.Status)
	if err != nil {
		return err
	}
	defer c.Close()
	st, p := c.store, c.phone
	if o.Revoke || o.Status {
		uid, err := st.UserID(p)
		if err != nil {
			return fmt.Errorf("no user with number %s", MaskPhone(p))
		}
		ts, err := st.ListAgentTokens(uid)
		if err != nil {
			return err
		}
		stamp := func(t int64) string { return time.Unix(t, 0).In(london).Format("2 Jan 2006 15:04 MST") }
		n := 0
		for _, t := range ts {
			if o.Revoke {
				if t.RevokedAt == nil && st.RevokeAgentToken(uid, t.ID) == nil {
					n++
				}
				continue
			}
			state, last := "active", "never"
			if t.RevokedAt != nil {
				state = "revoked " + stamp(*t.RevokedAt)
			}
			if t.LastSeenAt != nil {
				last = stamp(*t.LastSeenAt)
			}
			fmt.Fprintf(w, "#%d %q: %s. Created %s. Last seen %s.\n", t.ID, t.Label, state, stamp(t.CreatedAt), last)
		}
		if o.Revoke {
			fmt.Fprintf(w, "Revoked %d agent token(s) for %s.\n", n, MaskPhone(p))
		} else if len(ts) == 0 {
			fmt.Fprintf(w, "%s: no agent tokens.\n", MaskPhone(p))
		}
		return nil
	}
	uid, err := st.EnsureUser(p, c.dash)
	if err != nil {
		return err
	}
	raw, hash := newAgentToken()
	if _, err := st.CreateAgentToken(uid, "CLI", hash); err != nil {
		if errors.Is(err, ErrAgentTokenCap) {
			return fmt.Errorf("%s already has %d active agent tokens: revoke some first", MaskPhone(p), MaxAgentTokensPerUser)
		}
		return err
	}
	fmt.Fprintf(w, "Home agent token for %s. It is shown once and cannot be recovered.\nAnyone holding it can read your agent watches and report prices for them: treat it like a password.\n\n%s\n\nServer: %s\n", MaskPhone(p), raw, c.base)
	return nil
}
