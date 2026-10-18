package feed

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

type FeedDeviceKey struct {
	Pubkey    string     `json:"pubkey"`
	Label     string     `json:"label"`
	Created   time.Time  `json:"created"`
	RevokedAt *time.Time `json:"revoked_at"`
}

func (c *GroupClient) SigningDevices(ctx context.Context) ([]FeedDeviceKey, error) {
	var result struct {
		Devices []FeedDeviceKey `json:"devices"`
	}
	if err := c.request(ctx, http.MethodGet, "devices", nil, &result); err != nil {
		return nil, err
	}
	return result.Devices, nil
}

func (c *GroupClient) RevokeSigningDevice(ctx context.Context, pubkey string) error {
	raw, err := base64.StdEncoding.DecodeString(pubkey)
	if err != nil || len(raw) != 32 {
		return errors.New("feed: invalid signing pubkey")
	}
	return c.request(ctx, http.MethodPost, "devices/revoke", map[string]string{"pubkey": pubkey}, nil)
}

func newKeyCmd(env Env) *cobra.Command {
	key := &cobra.Command{Use: "key", Short: "manage account signing device keys"}
	key.AddCommand(&cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		client, err := NewGroupClient(env)
		if err != nil {
			return err
		}
		devices, err := client.SigningDevices(ctxOf(c))
		if err != nil {
			return err
		}
		for _, d := range devices {
			status := "active"
			if d.RevokedAt != nil {
				status = "revoked"
			}
			fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\n", d.Pubkey, status, d.Label)
		}
		return nil
	}})
	key.AddCommand(&cobra.Command{Use: "revoke PUBKEY", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		client, err := NewGroupClient(env)
		if err != nil {
			return err
		}
		if err := client.RevokeSigningDevice(ctxOf(c), args[0]); err != nil {
			return err
		}
		fmt.Fprintln(c.OutOrStdout(), "revoked", args[0])
		return nil
	}})
	return key
}
