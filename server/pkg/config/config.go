package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"
)

// DataTransport identifies how the server sends queries to D1.
type DataTransport string

const (
	DataTransportREST    DataTransport = "rest"
	DataTransportBinding DataTransport = "binding"
)

type Config struct {
	Server  *Server `yaml:"server"`
	Data    *Data   `yaml:"data"`
	Credits Credits `yaml:"credits"`
}

type Credits struct {
	FirstMarkReward            int64   `yaml:"first_mark_reward" json:"firstMarkReward"`
	InvitationRewards          []int64 `yaml:"invitation_rewards" json:"invitationRewards"`
	MaxConfirmationsPerAccount int     `yaml:"max_confirmations_per_account" json:"maxConfirmationsPerAccount"`
	MaxRewardPerAccount        int64   `yaml:"max_reward_per_account" json:"maxRewardPerAccount"`
	InvitationCost             int64   `yaml:"invitation_cost" json:"invitationCost"`
}

func DefaultCredits() Credits {
	return Credits{
		FirstMarkReward: 10, InvitationRewards: []int64{2, 1},
		MaxConfirmationsPerAccount: 3, MaxRewardPerAccount: 39, InvitationCost: 100,
	}
}

func (c Credits) Validate() error {
	if c.FirstMarkReward < 1 || c.FirstMarkReward > 1_000_000 {
		return fmt.Errorf("credits.first_mark_reward must be between 1 and 1000000")
	}
	if c.InvitationCost < 1 || c.InvitationCost > 1_000_000 {
		return fmt.Errorf("credits.invitation_cost must be between 1 and 1000000")
	}
	if len(c.InvitationRewards) > 5 {
		return fmt.Errorf("credits.invitation_rewards can include at most 5 invitation levels")
	}
	previous := c.FirstMarkReward
	for _, reward := range c.InvitationRewards {
		if reward < 1 || reward >= previous {
			return fmt.Errorf("invitation rewards must be positive and strictly decrease from first_mark_reward")
		}
		previous = reward
	}
	if c.MaxConfirmationsPerAccount < 1 || c.MaxConfirmationsPerAccount > 100 {
		return fmt.Errorf("credits.max_confirmations_per_account must be between 1 and 100")
	}
	if c.MaxRewardPerAccount < c.FirstMarkReward || c.MaxRewardPerAccount > 1_000_000 {
		return fmt.Errorf("credits.max_reward_per_account must cover the first mark reward and be at most 1000000")
	}
	return nil
}

type Server struct {
	Addr           string   `yaml:"addr"`
	SecureCookies  bool     `yaml:"secure_cookies"`
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// TrustedProxyPrefixes validates the IP addresses and CIDRs allowed to supply
// X-Forwarded-For. An empty list trusts no forwarding headers.
func (s Server) TrustedProxyPrefixes() ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(s.TrustedProxies))
	for _, value := range s.TrustedProxies {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			addr, addrErr := netip.ParseAddr(value)
			if addrErr != nil || addr.Zone() != "" {
				return nil, fmt.Errorf("server.trusted_proxies: %q must be an IP address or CIDR", value)
			}
			addr = addr.Unmap()
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return nil, fmt.Errorf("server.trusted_proxies: %q has an invalid IPv4-mapped prefix length", value)
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

type Data struct {
	Transport  DataTransport `yaml:"transport"`
	AccountID  string        `yaml:"account_id"`
	DatabaseID string        `yaml:"database_id"`
	TokenEnv   string        `yaml:"token_env"`
}

func ParseConfig(f string) (*Config, error) {
	return parseConfig(f, os.LookupEnv)
}

func parseConfig(f string, lookupEnv func(string) (string, bool)) (*Config, error) {
	_, err := os.Stat(f)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(f)
	if err != nil {
		return nil, err
	}
	conf := Config{Credits: DefaultCredits()}

	err = yaml.Unmarshal(data, &conf)
	if err != nil {
		return nil, err
	}
	if conf.Data != nil && conf.Data.Transport == "" {
		conf.Data.Transport = DataTransportREST
	}
	if err := conf.applyEnvironment(lookupEnv); err != nil {
		return nil, err
	}
	if err := conf.Validate(); err != nil {
		return nil, err
	}
	return &conf, nil
}

func (c *Config) applyEnvironment(lookupEnv func(string) (string, bool)) error {
	if c.Data != nil {
		if value, ok := lookupEnv("D1_TRANSPORT"); ok {
			c.Data.Transport = DataTransport(strings.TrimSpace(value))
		}
		if value, ok := lookupEnv("CLOUDFLARE_ACCOUNT_ID"); ok {
			c.Data.AccountID = value
		}
		if value, ok := lookupEnv("CLOUDFLARE_D1_DATABASE_ID"); ok {
			c.Data.DatabaseID = value
		} else if value, ok := lookupEnv("CLOUDFLARE_DATABASE_ID"); ok {
			c.Data.DatabaseID = value
		}
	}
	if c.Server == nil {
		return nil
	}
	if value, ok := lookupEnv("SERVER_SECURE_COOKIES"); ok {
		secure, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("SERVER_SECURE_COOKIES must be a boolean: %w", err)
		}
		c.Server.SecureCookies = secure
	}
	if value, ok := lookupEnv("SERVER_TRUSTED_PROXIES"); ok {
		c.Server.TrustedProxies = commaSeparatedValues(value)
	}
	return nil
}

func commaSeparatedValues(value string) []string {
	values := []string{}
	for entry := range strings.SplitSeq(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			values = append(values, entry)
		}
	}
	return values
}

func (c *Config) Validate() error {
	if c == nil || c.Server == nil {
		return fmt.Errorf("server configuration is required")
	}
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr is required")
	}
	if _, err := c.Server.TrustedProxyPrefixes(); err != nil {
		return err
	}
	if c.Data == nil {
		return fmt.Errorf("data configuration is required")
	}
	switch c.Data.Transport {
	case DataTransportBinding:
	case "", DataTransportREST:
		if strings.TrimSpace(c.Data.AccountID) == "" {
			return fmt.Errorf("data.account_id is required for REST transport")
		}
		if strings.TrimSpace(c.Data.DatabaseID) == "" {
			return fmt.Errorf("data.database_id is required for REST transport")
		}
		if strings.TrimSpace(c.Data.TokenEnv) == "" {
			return fmt.Errorf("data.token_env is required for REST transport")
		}
	default:
		return fmt.Errorf("data.transport must be %q or %q", DataTransportREST, DataTransportBinding)
	}
	return c.Credits.Validate()
}
