package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validData = "data:\n  account_id: account-id\n  database_id: database-id\n  token_env: CLOUDFLARE_API_KEY\n"

func noEnvironment(string) (string, bool) { return "", false }

func environment(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

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
		{name: "explicit D1 REST", data: "data:\n  transport: rest\n  account_id: account-id\n  database_id: database-id\n  token_env: CLOUDFLARE_API_KEY\n", valid: true},
		{name: "D1 binding", data: "data:\n  transport: binding\n", valid: true},
		{name: "unknown transport", data: "data:\n  transport: tunnel\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := "server:\n  addr: ':8090'\n" + tt.data
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := parseConfig(path, noEnvironment)
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
			conf, err := parseConfig(path, noEnvironment)
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
			conf, err := parseConfig(path, noEnvironment)
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

func TestEnvironmentOverrides(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		server         string
		data           string
		env            map[string]string
		wantTransport  DataTransport
		wantAccountID  string
		wantDatabaseID string
		wantSecure     bool
		wantProxies    []string
		wantError      string
	}{
		{
			name:   "Cloudflare binding",
			server: "server:\n  addr: ':8090'\n",
			data:   validData,
			env: map[string]string{
				"D1_TRANSPORT":           " binding ",
				"CLOUDFLARE_ACCOUNT_ID":  "",
				"CLOUDFLARE_DATABASE_ID": "",
				"SERVER_SECURE_COOKIES":  "true",
				"SERVER_TRUSTED_PROXIES": "0.0.0.0/0, ::/0",
			},
			wantTransport: DataTransportBinding,
			wantSecure:    true,
			wantProxies:   []string{"0.0.0.0/0", "::/0"},
		},
		{
			name:   "REST identifiers",
			server: "server:\n  addr: ':8090'\n  secure_cookies: true\n  trusted_proxies: [127.0.0.1]\n",
			data:   validData,
			env: map[string]string{
				"CLOUDFLARE_ACCOUNT_ID":  "production-account",
				"CLOUDFLARE_DATABASE_ID": "production-database",
				"SERVER_SECURE_COOKIES":  "false",
				"SERVER_TRUSTED_PROXIES": "",
			},
			wantTransport:  DataTransportREST,
			wantAccountID:  "production-account",
			wantDatabaseID: "production-database",
			wantProxies:    []string{},
		},
		{
			name:   "canonical D1 database identifier wins",
			server: "server:\n  addr: ':8090'\n",
			data:   validData,
			env: map[string]string{
				"CLOUDFLARE_D1_DATABASE_ID": "canonical-database",
				"CLOUDFLARE_DATABASE_ID":    "legacy-database",
			},
			wantTransport:  DataTransportREST,
			wantAccountID:  "account-id",
			wantDatabaseID: "canonical-database",
		},
		{
			name:      "invalid transport",
			server:    "server:\n  addr: ':8090'\n",
			data:      validData,
			env:       map[string]string{"D1_TRANSPORT": "tunnel"},
			wantError: "data.transport",
		},
		{
			name:      "empty REST account",
			server:    "server:\n  addr: ':8090'\n",
			data:      validData,
			env:       map[string]string{"CLOUDFLARE_ACCOUNT_ID": ""},
			wantError: "data.account_id",
		},
		{
			name:      "invalid secure cookies",
			server:    "server:\n  addr: ':8090'\n",
			data:      validData,
			env:       map[string]string{"SERVER_SECURE_COOKIES": "sometimes"},
			wantError: "SERVER_SECURE_COOKIES",
		},
		{
			name:      "invalid trusted proxy",
			server:    "server:\n  addr: ':8090'\n",
			data:      validData,
			env:       map[string]string{"SERVER_TRUSTED_PROXIES": "127.0.0.1,localhost"},
			wantError: "server.trusted_proxies",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.server+tt.data), 0600); err != nil {
				t.Fatal(err)
			}
			conf, err := parseConfig(path, environment(tt.env))
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("parseConfig error = %v, want error containing %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if conf.Data.Transport != tt.wantTransport {
				t.Errorf("transport = %q, want %q", conf.Data.Transport, tt.wantTransport)
			}
			if conf.Data.AccountID != tt.wantAccountID {
				t.Errorf("account ID = %q, want %q", conf.Data.AccountID, tt.wantAccountID)
			}
			if conf.Data.DatabaseID != tt.wantDatabaseID {
				t.Errorf("database ID = %q, want %q", conf.Data.DatabaseID, tt.wantDatabaseID)
			}
			if conf.Server.SecureCookies != tt.wantSecure {
				t.Errorf("secure cookies = %t, want %t", conf.Server.SecureCookies, tt.wantSecure)
			}
			if !reflect.DeepEqual(conf.Server.TrustedProxies, tt.wantProxies) {
				t.Errorf("trusted proxies = %v, want %v", conf.Server.TrustedProxies, tt.wantProxies)
			}
		})
	}
}
