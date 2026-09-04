package db

import (
	"strings"
	"testing"
)

func TestRedactDSN(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "password is replaced",
			in:   "postgres://qa:s3cr3t@127.0.0.1:5432/qa?sslmode=disable",
			want: "postgres://qa:xxxxx@127.0.0.1:5432/qa?sslmode=disable",
		},
		{
			name: "user without password is kept",
			in:   "postgres://qa@127.0.0.1:5432/qa",
			want: "postgres://qa@127.0.0.1:5432/qa",
		},
		{
			name: "no userinfo is kept",
			in:   "postgres://127.0.0.1:5432/qa",
			want: "postgres://127.0.0.1:5432/qa",
		},
		{
			name: "unparsable input is not echoed",
			in:   "postgres://qa:s3cr3t@127.0.0.1:5432/qa\x7f",
			want: "[unparsable dsn]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactDSN(tt.in)
			if got != tt.want {
				t.Fatalf("RedactDSN(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if strings.Contains(got, "s3cr3t") {
				t.Fatalf("RedactDSN leaked the password: %q", got)
			}
		})
	}
}

func TestDSNFromEnv(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if _, err := DSNFromEnv(); err == nil {
		t.Fatal("expected an error when DATABASE_URL is unset")
	}

	t.Setenv("DATABASE_URL", "postgres://qa@127.0.0.1:5432/qa")
	got, err := DSNFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "postgres://qa@127.0.0.1:5432/qa" {
		t.Fatalf("unexpected dsn: %q", got)
	}
}
