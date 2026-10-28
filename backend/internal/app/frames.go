package app

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"context"
	"math"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Frames group nodes on the connect canvas (domain.TopologyFrame). Drawing them is arranging the board for everyone,
// so it is kept to owners and admins; the database narrows it further to the caller's projects.

// FrameInput is what a caller sends to create or change a frame.
type FrameInput struct {
	Name       string
	Color      string
	X, Y, W, H float64
}

// frame validates and normalises an input; positions are kept to two decimals, like layout positions.
func (in FrameInput) frame() (domain.TopologyFrame, error) {
	r := func(v float64) float64 { return math.Round(v*100) / 100 }
	f := domain.TopologyFrame{Name: strings.TrimSpace(in.Name), Color: in.Color, X: r(in.X), Y: r(in.Y), W: r(in.W), H: r(in.H)}
	if !validName(f.Name) || !slices.Contains(domain.ProjectColors, f.Color) || !f.ValidGeometry() {
		return f, domain.ErrInvalid
	}
	return f, nil
}

func (s *Service) CreateFrame(ctx context.Context, p domain.Principal, view string, project *string, in FrameInput) (domain.TopologyFrame, error) {
	if !p.CanManageDevices() {
		return domain.TopologyFrame{}, domain.ErrForbidden
	}
	f, e := in.frame()
	if e != nil || !domain.ValidLayoutView(view) || (project != nil && !security.ValidID(*project)) {
		return domain.TopologyFrame{}, domain.ErrInvalid
	}
	f.ID, f.View, f.ProjectID = uuid.NewString(), view, project
	return s.Repo.CreateFrame(ctx, p, f)
}

func (s *Service) UpdateFrame(ctx context.Context, p domain.Principal, id string, in FrameInput) (domain.TopologyFrame, error) {
	if !p.CanManageDevices() {
		return domain.TopologyFrame{}, domain.ErrForbidden
	}
	f, e := in.frame()
	if e != nil || !security.ValidID(id) {
		return domain.TopologyFrame{}, domain.ErrInvalid
	}
	f.ID = id
	return s.Repo.UpdateFrame(ctx, p, f)
}

func (s *Service) DeleteFrame(ctx context.Context, p domain.Principal, id string) error {
	if !p.CanManageDevices() {
		return domain.ErrForbidden
	}
	if !security.ValidID(id) {
		return domain.ErrInvalid
	}
	return s.Repo.DeleteFrame(ctx, p, id)
}
