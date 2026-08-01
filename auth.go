package main

import "net/http"

// Member is whoever is submitting content. The members' app is meant to be the
// source of this identity: it already knows who is a member, and its name is
// what lands in the `members` front matter of the resulting page.
type Member struct {
	Name string
}

// Authenticator resolves the member behind a request.
//
// This is an interface with no real implementation yet on purpose.
// app.makespace.org exposes no OAuth or OIDC endpoint — just its own /log-in —
// so there is nothing for the server to verify a session against. Bridging that
// needs a change on the members' app side; see CLAUDE.md for the two options.
type Authenticator interface {
	Member(r *http.Request) (Member, bool)
}

// deniedAuth refuses every request. It is the default deliberately: an open
// submission endpoint would let anyone push objects into a public bucket and
// open pull requests, which is worse than a form that is not wired up yet.
type deniedAuth struct{}

func (deniedAuth) Member(*http.Request) (Member, bool) { return Member{}, false }

// devMember stands in for the members' app locally: -dev-member "Riley P".
type devMember struct{ name string }

func (d devMember) Member(*http.Request) (Member, bool) {
	return Member{Name: d.name}, d.name != ""
}
