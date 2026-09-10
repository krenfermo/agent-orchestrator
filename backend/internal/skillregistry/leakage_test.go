package skillregistry_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry/registrytest"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillsecrets"
)

// leakage_test.go -- the credential must not appear anywhere but the request
// header it was resolved for.
//
// These are the tests that would have caught it if somebody had written
// `fmt.Errorf("auth failed for %v", creds)` or put Credentials in a struct that
// gets serialized into a response. They are cheap and they are the only thing
// standing between a redacting type and a redacting type somebody bypassed.

const canary = "canary-CREDENTIAL-must-not-appear-anywhere"

func resolverReturning(value string) skillregistry.SecretResolver {
	return skillregistry.SecretResolverFunc(
		func(context.Context, skillregistry.Registry) (skillsecrets.SecretValue, error) {
			return skillsecrets.NewSecretValue(value), nil
		})
}

// TestCredentialsNeverRenderTheValue covers every rendering path Go offers a
// struct: %v, %s, %+v, %#v and encoding/json.
func TestCredentialsNeverRenderTheValue(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	srv.RequireBearer(canary)

	reg := srv.Registry("corp")
	reg.AuthType = skillregistry.AuthBearer
	reg.CredentialSecretName = "CORP_TOKEN"

	p, err := skillregistry.NewHTTPSProvider(
		context.Background(), reg, resolverReturning(canary), srv.Options())
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	// The provider holds the credential. Rendering the provider, the registry
	// and the whole request path must not produce it.
	renderings := []string{
		fmt.Sprintf("%v", p),
		fmt.Sprintf("%+v", p),
		fmt.Sprintf("%#v", p),
		fmt.Sprintf("%s", p),
		fmt.Sprintf("%v", reg),
		fmt.Sprintf("%#v", reg),
	}
	if b, err := json.Marshal(reg); err == nil {
		renderings = append(renderings, string(b))
	}
	for i, r := range renderings {
		if strings.Contains(r, canary) {
			t.Fatalf("rendering %d leaked the credential: %s", i, r)
		}
	}
	if _, err := p.Search(context.Background(), skillregistry.Query{}); err != nil {
		t.Fatalf("the canary credential did not actually work: %v", err)
	}
}

// TestErrorsNameTheRefAndNotTheValue walks the failures an operator will
// actually see and checks each message.
func TestErrorsNameTheRefAndNotTheValue(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	srv.RequireBearer("the-real-token")

	reg := srv.Registry("corp")
	reg.AuthType = skillregistry.AuthBearer
	reg.CredentialSecretName = "CORP_TOKEN"

	cases := []struct {
		name string
		// namesRef is false only where the failure happens before the
		// credential is any part of the story -- a certificate that does not
		// verify is refused by the transport, and naming a secret there would
		// point an operator at the wrong thing.
		namesRef bool
		run      func() error
	}{
		{"a rejected credential", true, func() error {
			p, err := skillregistry.NewHTTPSProvider(
				context.Background(), reg, resolverReturning(canary), srv.Options())
			if err != nil {
				return err
			}
			_, err = p.Search(context.Background(), skillregistry.Query{})
			return err
		}},
		{"a certificate for another name", false, func() error {
			other := reg
			other.Location = "https://elsewhere.example.com"
			p, err := skillregistry.NewHTTPSProvider(
				context.Background(), other, resolverReturning(canary), srv.Options())
			if err != nil {
				return err
			}
			_, err = p.Search(context.Background(), skillregistry.Query{})
			return err
		}},
		{"a secret that will not resolve", true, func() error {
			failing := skillregistry.SecretResolverFunc(
				func(context.Context, skillregistry.Registry) (skillsecrets.SecretValue, error) {
					return skillsecrets.SecretValue{}, errors.New("no such secret")
				})
			_, err := skillregistry.NewHTTPSProvider(context.Background(), reg, failing, srv.Options())
			return err
		}},
		{"an empty secret", true, func() error {
			_, err := skillregistry.NewHTTPSProvider(
				context.Background(), reg, resolverReturning(""), srv.Options())
			return err
		}},
		{"no secret backend at all", true, func() error {
			_, err := skillregistry.NewHTTPSProvider(context.Background(), reg, nil, srv.Options())
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("the error quotes the credential: %v", err)
			}
			if tc.namesRef && !strings.Contains(err.Error(), "CORP_TOKEN") {
				t.Fatalf("the error does not name the secretRef: %v", err)
			}
		})
	}
}

// TestNoSecretUnavailableErrorEverHoldsAValue is the belt to the braces above:
// the sentinel's own message is checked, so a future wrapper that includes the
// value has one more thing to walk past.
func TestNoSecretUnavailableErrorEverHoldsAValue(t *testing.T) {
	if strings.Contains(skillregistry.ErrSecretUnavailable.Error(), "value") {
		t.Fatal("the sentinel's own text suggests it carries one")
	}
}

// TestCredentialIsNotSentToARedirectTarget proves the header does not travel
// even one hop. The redirect is refused outright, so the fixture at the other
// end never sees a request at all -- which is a stronger property than
// "stripped on cross-origin".
func TestCredentialIsNotSentToARedirectTarget(t *testing.T) {
	victim := registrytest.New(t, "victim")
	attacker := registrytest.New(t, "attacker")
	victim.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	rel := victim.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	victim.RequireBearer(canary)
	victim.Fail(registrytest.Failures{RedirectArtifactTo: attacker.BaseURL() + "/v1/skills"})

	reg := victim.Registry("victim")
	reg.AuthType = skillregistry.AuthBearer
	reg.CredentialSecretName = "CORP_TOKEN"
	p, err := skillregistry.NewHTTPSProvider(
		context.Background(), reg, resolverReturning(canary), victim.Options())
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	if err := p.FetchArtifact(context.Background(), rel, t.TempDir()); !errors.Is(err, skillregistry.ErrNetworkPolicy) {
		t.Fatalf("the redirect gave %v, want ErrNetworkPolicy", err)
	}
	if len(attacker.Requests()) != 0 {
		t.Fatalf("the redirect target was contacted: %v", attacker.Requests())
	}
}
