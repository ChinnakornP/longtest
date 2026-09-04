package project

import "testing"

// base_url is rendered in the UI, handed to a daemon and written into an
// application map, so what this accepts is what all three end up trusting.
func TestNormaliseBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "https", raw: "https://demo.example.com", want: "https://demo.example.com"},
		{name: "a trailing slash is dropped", raw: "https://demo.example.com/", want: "https://demo.example.com"},
		{name: "a sub-path is kept", raw: "https://demo.example.com/app", want: "https://demo.example.com/app"},
		{name: "the scheme is lower-cased", raw: "HTTPS://Demo.Example.COM", want: "https://demo.example.com"},
		{name: "a local target", raw: "http://localhost:3000", want: "http://localhost:3000"},
		{name: "an internal address", raw: "http://192.168.1.20", want: "http://192.168.1.20"},
		{name: "surrounding whitespace", raw: "  https://demo.example.com  ", want: "https://demo.example.com"},
		// A query string and a fragment are not part of an origin, and keeping
		// them would mean two spellings of the same target.
		{name: "a query string is dropped", raw: "https://demo.example.com/app?tab=1", want: "https://demo.example.com/app"},
		{name: "a fragment is dropped", raw: "https://demo.example.com/app#top", want: "https://demo.example.com/app"},

		{name: "empty", raw: "", wantErr: true},
		{name: "no scheme", raw: "demo.example.com", wantErr: true},
		{name: "not http", raw: "ftp://demo.example.com", wantErr: true},
		{name: "a file url", raw: "file:///etc/passwd", wantErr: true},
		{name: "javascript", raw: "javascript:alert(1)", wantErr: true},
		// Credentials in a URL would be rendered, logged and handed to a daemon.
		{name: "carries credentials", raw: "https://admin:hunter2@demo.example.com", wantErr: true},
		{name: "carries a username", raw: "https://admin@demo.example.com", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normaliseBaseURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normaliseBaseURL(%q) accepted it as %q", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normaliseBaseURL(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("normaliseBaseURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestValidateName(t *testing.T) {
	if err := validateName("Demo"); err != nil {
		t.Fatalf("rejected a normal name: %v", err)
	}
	if err := validateName(""); err == nil {
		t.Fatal("accepted an empty name")
	}
	long := make([]byte, maxProjectNameLength+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := validateName(string(long)); err == nil {
		t.Fatal("accepted a name past the column's limit")
	}
}
