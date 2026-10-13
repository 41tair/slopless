package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validData = "data:\n  account_id: account-id\n  database_id: database-id\n  token_env: CLOUDFLARE_API_KEY\n"

func TestDataConfiguration(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		data  string
		valid bool
	}{
		{name: "missing data"},
		{name: "missing account ID", data: "data:\n  database_id: database-id\n  token_env: CLOUDFLARE_API_KEY\n"},
		{name: "empty account ID", data: "data:\n  account_id: '   '\n  database_id: database-id\n  token_env: CLOUDFLARE_API_KEY\n"},
		{name: "missing database ID", data: "data:\n  account_id: account-id\n  token_env: CLOUDFLARE_API_KEY\n"},
		{name: "empty database ID", data: "data:\n  account_id: account-id\n  database_id: '   '\n  token_env: CLOUDFLARE_API_KEY\n"},
		{name: "missing token environment", data: "data:\n  account_id: account-id\n  database_id: database-id\n"},
		{name: "D1 REST", data: validData, valid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := "server:\n  addr: ':8090'\n" + tt.data
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := ParseConfig(path)
			if (err == nil) != tt.valid {
				t.Fatalf("ParseConfig error = %v, valid = %v", err, tt.valid)
			}
		})
	}
}

func TestCreditConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, extra string
		valid       bool
	}{
		{name: "defaults", valid: true},
		{name: "custom price", extra: "credits:\n  invitation_cost: 250\n", valid: true},
		{name: "free invitations rejected", extra: "credits:\n  invitation_cost: 0\n"},
		{name: "negative primary reward", extra: "credits:\n  first_mark_reward: -1\n"},
		{name: "growing referral rewards rejected", extra: "credits:\n  invitation_rewards: [1, 2]\n"},
		{name: "too many levels", extra: "credits:\n  invitation_rewards: [6, 5, 4, 3, 2, 1]\n"},
		{name: "zero confirmation cap", extra: "credits:\n  max_confirmations_per_account: 0\n"},
		{name: "insufficient target budget", extra: "credits:\n  max_reward_per_account: 1\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := "server:\n  addr: ':8090'\n" + validData + tt.extra
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			conf, err := ParseConfig(path)
			if (err == nil) != tt.valid {
				t.Fatalf("ParseConfig error = %v, valid = %v", err, tt.valid)
			}
			if tt.name == "defaults" && !reflect.DeepEqual(conf.Credits, DefaultCredits()) {
				t.Fatalf("wrong defaults: %+v", conf.Credits)
			}
			if tt.name == "custom price" && conf.Credits.InvitationCost != 250 {
				t.Fatal("price override ignored")
			}
		})
	}
}

func TestTrustedProxyConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		values  []string
		want    []string
		invalid bool
	}{
		{name: "disabled", want: []string{}},
		{name: "addresses", values: []string{"127.0.0.1", "::1"}, want: []string{"127.0.0.1/32", "::1/128"}},
		{name: "CIDRs", values: []string{"10.0.0.5/24", "2001:db8::1/64"}, want: []string{"10.0.0.0/24", "2001:db8::/64"}},
		{name: "mapped IPv4", values: []string{"::ffff:127.0.0.1", "::ffff:10.0.0.0/120"}, want: []string{"127.0.0.1/32", "10.0.0.0/24"}},
		{name: "hostname", values: []string{"localhost"}, invalid: true},
		{name: "invalid mask", values: []string{"127.0.0.1/33"}, invalid: true},
		{name: "port", values: []string{"127.0.0.1:8090"}, invalid: true},
		{name: "zone", values: []string{"fe80::1%en0"}, invalid: true},
		{name: "invalid mapped mask", values: []string{"::ffff:10.0.0.0/80"}, invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := "server:\n  addr: ':8090'\n  trusted_proxies: [" + strings.Join(tt.values, ", ") + "]\n" + validData
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			conf, err := ParseConfig(path)
			if tt.invalid {
				if err == nil || !strings.Contains(err.Error(), "server.trusted_proxies") {
					t.Fatalf("invalid trusted proxies returned %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			prefixes, err := conf.Server.TrustedProxyPrefixes()
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(prefixes))
			for _, prefix := range prefixes {
				got = append(got, prefix.String())
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("prefixes = %v, want %v", got, tt.want)
			}
		})
	}
}
