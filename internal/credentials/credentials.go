// Package credentials resolves explicitly configured environment references.
package credentials

import (
	"context"
	"errors"
	"os"
)

type Credential struct {
	Community string `json:"-"`
	Username  string `json:"-"`
	Password  string `json:"-"`
}

func (Credential) String() string   { return "[redacted credential]" }
func (Credential) GoString() string { return "[redacted credential]" }

type Reference struct {
	CommunityEnv string `yaml:"community_env" json:"-"`
	UsernameEnv  string `yaml:"username_env" json:"-"`
	PasswordEnv  string `yaml:"password_env" json:"-"`
}
type Provider interface {
	Get(context.Context, string) (Credential, error)
}
type Environment struct{ references map[string]Reference }

func NewEnvironment(refs map[string]Reference) *Environment {
	p := &Environment{references: make(map[string]Reference, len(refs))}
	for key, value := range refs {
		p.references[key] = value
	}
	return p
}
func (p *Environment) Get(ctx context.Context, ref string) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	r, ok := p.references[ref]
	if !ok {
		return Credential{}, errors.New("credential reference unavailable")
	}
	read := func(name string) (string, bool) {
		if name == "" {
			return "", true
		}
		v, ok := os.LookupEnv(name)
		return v, ok && v != ""
	}
	c, a := read(r.CommunityEnv)
	u, b := read(r.UsernameEnv)
	s, d := read(r.PasswordEnv)
	if !a || !b || !d {
		return Credential{}, errors.New("credential environment unavailable")
	}
	return Credential{Community: c, Username: u, Password: s}, nil
}
