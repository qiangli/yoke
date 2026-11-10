package fleet

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// SatelliteDetail is the caller-specific detail view. Hosts is owner-only;
// Attached is present for host credentials, including when false.
type SatelliteDetail struct {
	SatelliteEntry
	OwnerEmail string   `json:"owner_email,omitempty"`
	Hosts      []string `json:"hosts,omitempty"`
	Attached   *bool    `json:"attached,omitempty"`
}

// Satellite reads one satellite by ID without fetching the catalog.
func (c OwnerClient) Satellite(ctx context.Context, id string) (SatelliteDetail, error) {
	sat, _, err := c.satellite(ctx, id)
	return sat, err
}

func (c OwnerClient) satellite(ctx context.Context, id string) (SatelliteDetail, int, error) {
	var env struct {
		Satellite SatelliteDetail `json:"satellite"`
	}
	status, err := c.requestStatus(ctx, http.MethodGet, "/api/v1/satellites/"+url.PathEscape(id), nil, &env)
	return env.Satellite, status, err
}

func (c OwnerClient) satelliteByKey(ctx context.Context, key string) (SatelliteDetail, error) {
	sat, status, err := c.satellite(ctx, key)
	if status != http.StatusNotFound {
		return sat, err
	}
	rows, err := c.Satellites(ctx)
	if err != nil {
		return SatelliteDetail{}, err
	}
	row, err := selectSatellite(rows, key)
	if err != nil {
		return SatelliteDetail{}, err
	}
	return c.Satellite(ctx, row.ID)
}

// SatelliteAssignment includes pending as well as attached hosts.
type SatelliteAssignment struct {
	Host        string     `json:"host"`
	SatelliteID string     `json:"satellite_id"`
	State       string     `json:"state"`
	AssignedAt  time.Time  `json:"assigned_at"`
	AttachedAt  *time.Time `json:"attached_at"`
}

// SatelliteAssignments reads the caller-visible assignments, optionally
// filtered by satellite ID on the server.
func (c OwnerClient) SatelliteAssignments(ctx context.Context, id string) ([]SatelliteAssignment, error) {
	path := "/api/v1/satellites/assignment"
	if id != "" {
		path += "?satellite=" + url.QueryEscape(id)
	}
	var env struct {
		Assignments []SatelliteAssignment `json:"assignments"`
	}
	err := c.request(ctx, http.MethodGet, path, nil, &env)
	if err != nil {
		return nil, fmt.Errorf("fleet: GET /api/v1/satellites/assignment: %w", err)
	}
	return env.Assignments, nil
}

// DeleteSelf removes the authenticated host and its satellite assignments.
// Use a host token on the master or a satellite-host token on the satellite.
// A revoked token returns 401 on retries, which the contract treats as done.
func (c OwnerClient) DeleteSelf(ctx context.Context) error {
	status, err := c.requestStatus(ctx, http.MethodDelete, "/api/v1/hosts/self", nil, nil)
	if status == http.StatusUnauthorized {
		return nil
	}
	return err
}
