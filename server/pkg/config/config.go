package config

import (
	"fmt"
	"net/netip"
	"os"
	"strings"

	"go.yaml.in/yaml/v4"
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
	AccountID  string `yaml:"account_id"`
	DatabaseID string `yaml:"database_id"`
	TokenEnv   string `yaml:"token_env"`
}

func ParseConfig(f string) (*Config, error) {
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
	if err := conf.Validate(); err != nil {
		return nil, err
	}
	return &conf, nil
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
	if strings.TrimSpace(c.Data.AccountID) == "" {
		return fmt.Errorf("data.account_id is required")
	}
	if strings.TrimSpace(c.Data.DatabaseID) == "" {
		return fmt.Errorf("data.database_id is required")
	}
	if c.Data.TokenEnv == "" {
		return fmt.Errorf("data.token_env is required")
	}
	return c.Credits.Validate()
}
