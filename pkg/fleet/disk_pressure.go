package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"

	"github.com/spf13/cobra"
)

type diskPressureStatus struct {
	Host    string `json:"host"`
	Volumes []struct {
		ID          string               `json:"id"`
		DisplayName string               `json:"display_name"`
		ObservedAt  string               `json:"observed_at"`
		Used        uint64               `json:"used_bytes"`
		Total       uint64               `json:"total_bytes"`
		Growers     []diskPressureGrower `json:"growers"`
		Alerts      []struct {
			Threshold int                  `json:"threshold"`
			ID        string               `json:"alert_id"`
			Delivery  string               `json:"delivery"`
			Growers   []diskPressureGrower `json:"growers"`
		} `json:"alerts"`
	} `json:"volumes"`
}

type diskPressureGrower struct {
	Label string `json:"label"`
	Delta uint64 `json:"delta_bytes"`
}

func newDiskPressureCmd(clientFor func(context.Context) (PairedHostsClient, error), asJSON *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "disk-pressure HOST",
		Short: "Show disk measurements and alerts from the owning satellite",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := clientFor(cmd.Context())
			if err != nil {
				return err
			}
			path := "/api/v1/hosts/" + url.PathEscape(args[0]) + "/disk-pressure"
			raw, err := client.hostStatusRequest(cmd.Context(), path, nil)
			if err != nil {
				return err
			}
			return printDiskPressure(cmd.OutOrStdout(), raw, *asJSON)
		},
	}
}

func printDiskPressure(out io.Writer, raw []byte, asJSON bool) error {
	var status diskPressureStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return err
	}
	if status.Host == "" {
		return fmt.Errorf("invalid disk pressure response")
	}
	if asJSON {
		_, err := fmt.Fprintln(out, string(bytes.TrimSpace(raw)))
		return err
	}
	if len(status.Volumes) == 0 {
		_, err := fmt.Fprintf(out, "%s\tno disk observations\n", status.Host)
		return err
	}
	var text bytes.Buffer
	fmt.Fprintln(&text, status.Host)
	for _, v := range status.Volumes {
		name := v.ID
		if v.DisplayName != "" {
			name = fmt.Sprintf("%s (%s)", v.DisplayName, v.ID)
		}
		fmt.Fprintf(&text, "%s\t%d/%d bytes used", name, v.Used, v.Total)
		if v.ObservedAt != "" {
			fmt.Fprintf(&text, "\tobserved %s", v.ObservedAt)
		}
		fmt.Fprintln(&text)
		for _, g := range v.Growers {
			fmt.Fprintf(&text, "  %s\t+%d bytes\n", g.Label, g.Delta)
		}
		for _, alert := range v.Alerts {
			fmt.Fprintf(&text, "  alert %s\t%d%%\t%s\n", alert.ID, alert.Threshold, alert.Delivery)
			for _, g := range alert.Growers {
				fmt.Fprintf(&text, "    %s\t+%d bytes at crossing\n", g.Label, g.Delta)
			}
		}
	}
	_, err := out.Write(text.Bytes())
	return err
}
