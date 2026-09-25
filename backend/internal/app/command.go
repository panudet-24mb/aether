package app

import (
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"context"
)

// SendCommand asks for one property of a device to be set ("set" with a value) or a binary property to be
// flipped ("toggle"). Only owner, admin and operator may; the database narrows it to the member's projects and
// validates the value against the device's own Zigbee2MQTT definition. The command is queued, not executed:
// mqtt-commander publishes it and the device's next state report confirms it (docs/platform/zigbee2mqtt.md).
func (s *Service) SendCommand(ctx context.Context, p domain.Principal, req domain.CommandRequest) (domain.Command, bool, error) {
	if !p.CanCommandDevices() {
		return domain.Command{}, false, domain.ErrForbidden
	}
	// The module restriction is enforced here too, not only by the HTTP gate: the rule must hold whatever
	// route or caller reaches the service.
	if p.Role != "owner" {
		access, e := s.Repo.MemberAccess(ctx, p, p.UserID)
		if e != nil {
			return domain.Command{}, false, e
		}
		if !p.MayControl(access) {
			return domain.Command{}, false, domain.ErrForbidden
		}
	}
	if !security.ValidID(req.ID) || !security.ValidID(req.DeviceID) || !zigbee2mqtt.PropertyPattern.MatchString(req.Property) {
		return domain.Command{}, false, domain.ErrInvalid
	}
	switch req.Action {
	case "set":
		if len(req.Value) == 0 || len(req.Value) > 1024 {
			return domain.Command{}, false, domain.Because(domain.ErrInvalid, "value")
		}
	case "toggle":
		if len(req.Value) != 0 {
			return domain.Command{}, false, domain.Because(domain.ErrInvalid, "value")
		}
	default:
		return domain.Command{}, false, domain.ErrInvalid
	}
	return s.Repo.QueueCommand(ctx, p, req)
}
