package app

import (
	"aether/backend/internal/domain"
	"context"
	"math"
)

// SaveRoute stores the bend points of one connection on the canvas (domain.EdgeRoute); no points draws it straight
// again. Routing lines is arranging the board, so it is the same write as moving nodes (owner, admin, operator).
func (s *Service) SaveRoute(ctx context.Context, p domain.Principal, view, edge string, points []domain.CanvasPoint) error {
	if !p.CanOperate() {
		return domain.ErrForbidden
	}
	kind, ref, ok := domain.ParseRouteEdge(edge)
	if !ok || !domain.ValidLayoutView(view) || len(points) > domain.MaxRoutePoints {
		return domain.ErrInvalid
	}
	clean := make([]domain.CanvasPoint, len(points))
	for i, pt := range points {
		// Kept to two decimals, like node positions.
		clean[i] = domain.CanvasPoint{X: math.Round(pt.X*100) / 100, Y: math.Round(pt.Y*100) / 100}
		if !clean[i].Valid() {
			return domain.ErrInvalid
		}
	}
	return s.Repo.SaveRoute(ctx, p, view, kind, ref, clean)
}
