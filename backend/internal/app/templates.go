package app

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"context"
	"encoding/hex"
	"github.com/google/uuid"
	"math"
	"strings"
	"time"
)

func (s *Service) CreateTemplate(ctx context.Context, p domain.Principal, t domain.DeviceTemplate) (domain.DeviceTemplate, error) {
	if !p.CanManageDevices() {
		return t, domain.ErrForbidden
	}
	t.Name = strings.TrimSpace(t.Name)
	if !validName(t.Name) || t.Version < 1 || t.Version > 100000 || t.DecoderID != "minew-ffe1-a101@1" {
		return t, domain.ErrInvalid
	}
	for _, v := range []*float64{t.Definition.TemperatureHigh, t.Definition.HumidityHigh} {
		if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0)) {
			return t, domain.ErrInvalid
		}
	}
	if v := t.Definition.TemperatureHigh; v != nil && (*v < -100 || *v > 125) {
		return t, domain.ErrInvalid
	}
	if v := t.Definition.HumidityHigh; v != nil && (*v < 0 || *v > 100) {
		return t, domain.ErrInvalid
	}
	t.ID = uuid.NewString()
	t.CreatedAt = time.Now().UTC()
	return t, s.Repo.CreateTemplate(ctx, p, t)
}
func (s *Service) AssignTemplate(ctx context.Context, p domain.Principal, gateway, external, name, template string) error {
	if !p.CanManageDevices() {
		return domain.ErrForbidden
	}
	mac, e := hex.DecodeString(external)
	if e != nil || len(mac) != 6 || !security.ValidID(gateway) || !security.ValidID(template) || !validName(strings.TrimSpace(name)) {
		return domain.ErrInvalid
	}
	return s.Repo.SetStreamTemplate(ctx, p, gateway, strings.ToLower(external), strings.TrimSpace(name), template)
}
