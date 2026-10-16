package domain

import (
	"slices"
	"strings"
	"time"
)

// Reports and data export (docs/platform/reports.md): CSV of one or more fixed sensors, and summary reports with
// excursions out of an allowed range. Wearables and roaming tags are personal data and belong to the privacy export.

const (
	ReportMaxSensors     = 50
	ReportMaxWindow      = 31 * 24 * time.Hour
	ReportMaxDefinitions = 200
	ReportNameMax        = 120
)

// ReportRawSensorDays caps a raw export at sensors x days (days started count whole). At SAMPLE_MIN_INTERVAL_SEC=30 a
// sensor stores 2,880 samples a day and a raw CSV row is about 110 bytes, so one sensor-day is about 0.3 MB and the cap
// about 38 MB: a file the browser can still hold in memory and save. Aggregated resolutions are not capped this way
// (a month of 5-minute buckets of 50 sensors is about 430,000 rows, ~45 MB, and far fewer for 1h and 1d).
const ReportRawSensorDays = 120

// ValidRawBudget refuses a raw export beyond ReportRawSensorDays.
func ValidRawBudget(sensors int, from, to time.Time) error {
	days := int((to.Sub(from) + 24*time.Hour - 1) / (24 * time.Hour))
	if sensors*days > ReportRawSensorDays {
		return Because(ErrInvalid, "raw_too_large")
	}
	return nil
}

// ReportResolutions: raw samples, or 5-minute / hourly / daily (Bangkok calendar day) rollup buckets.
var ReportResolutions = []string{"raw", "5m", "1h", "1d"}

// ReportSensor names one sensor stream: the gateway that hears it and its external id.
type ReportSensor struct {
	GatewayID  string `json:"gateway_id"`
	ExternalID string `json:"external_id"`
}

// ReportSensorInfo is a sensor the caller may report on, with the names the CSV and the report print.
type ReportSensorInfo struct {
	GatewayID   string  `json:"gateway_id"`
	ExternalID  string  `json:"external_id"`
	Name        string  `json:"name"`
	GatewayName string  `json:"gateway_name"`
	ProjectID   *string `json:"project_id"`
	ProfileID   string  `json:"profile_id"`
}

// ReportRow is one CSV row: a raw sample (Temperature, Humidity, Battery) or an aggregated bucket (N and the rest).
type ReportRow struct {
	At          time.Time
	Temperature *float64
	Humidity    *float64
	Battery     *float64
	N           int
	TAvg        *float64
	TMin        *float64
	TMax        *float64
	HAvg        *float64
	HMin        *float64
	HMax        *float64
}

// ReportDefinition is a saved summary report. ProjectID nil is a workspace report.
type ReportDefinition struct {
	ID        string         `json:"id"`
	ProjectID *string        `json:"project_id"`
	Name      string         `json:"name"`
	Kind      string         `json:"kind"`
	Sensors   []ReportSensor `json:"sensors"`
	TempMin   *float64       `json:"temp_min"`
	TempMax   *float64       `json:"temp_max"`
	HumMin    *float64       `json:"hum_min"`
	HumMax    *float64       `json:"hum_max"`
	CreatedBy *string        `json:"created_by"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	// Schedule (docs/platform/reports.md, "Scheduled delivery"): none | daily | weekly, an ISO weekday for weekly,
	// a local Bangkok time "HH:MM", the notification channels, and when the scheduler next sends it.
	Schedule        string     `json:"schedule"`
	ScheduleWeekday *int       `json:"schedule_weekday"`
	ScheduleTime    *string    `json:"schedule_time"`
	Channels        []string   `json:"channels"`
	NextRunAt       *time.Time `json:"next_run_at"`
}

// ReportManualPerHour is how many reports one workspace may send by hand in an hour.
const ReportManualPerHour = 10

// ReportMaxChannels is how many channels one report goes to.
const ReportMaxChannels = 10

// ReportOmitted is a saved report's sensor left out of a run, and why (sensor_not_found, personal_sensor, ...).
type ReportOmitted struct {
	ReportSensor
	Reason string `json:"reason"`
}

// ReportRun is one sending of a saved report: the period it covers and its deliveries, one per channel.
type ReportRun struct {
	ID         string           `json:"id"`
	ReportID   string           `json:"report_id"`
	ProjectID  *string          `json:"project_id"`
	PeriodFrom time.Time        `json:"period_from"`
	PeriodTo   time.Time        `json:"period_to"`
	Trigger    string           `json:"trigger"` // schedule | manual
	Status     string           `json:"status"`  // queued | done
	CreatedBy  *string          `json:"created_by"`
	CreatedAt  time.Time        `json:"created_at"`
	FinishedAt *time.Time       `json:"finished_at"`
	Deliveries []ReportDelivery `json:"deliveries"`
}

// ReportDelivery is a run's delivery to one channel: queued | sent | failed | skipped.
type ReportDelivery struct {
	ID            string     `json:"id"`
	ChannelID     *string    `json:"channel_id"`
	ChannelName   string     `json:"channel_name"`
	ChannelKind   string     `json:"channel_kind"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	LastError     *string    `json:"last_error"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	SentAt        *time.Time `json:"sent_at"`
}

// ReportDeliveryJob is a claimed delivery with what sending it needs. FailReason ends it without sending; Skip ends
// it as skipped; RetryReason leaves it for the usual backoff.
type ReportDeliveryJob struct {
	Delivery    ReportDelivery
	Run         ReportRun
	Channel     NotificationChannel
	SecretEnc   string
	FailReason  string
	RetryReason string
}

// ReportRunContent is what a run's messages are made of: the report as it is now, the sensors that still resolve
// (and the ones left out), the workspace's name.
type ReportRunContent struct {
	Run        ReportRun
	Definition ReportDefinition
	Sensors    []ReportSensorInfo
	Omitted    []ReportOmitted
	Workspace  string
}

// ValidReportWindow checks a report range: from before to, at most ReportMaxWindow.
func ValidReportWindow(from, to time.Time) error {
	if from.IsZero() || to.IsZero() || !from.Before(to) {
		return Because(ErrInvalid, "invalid_range")
	}
	if to.Sub(from) > ReportMaxWindow {
		return Because(ErrInvalid, "window_too_long")
	}
	return nil
}

// NormaliseReportSensors checks a sensor list (1..ReportMaxSensors, no duplicates, well-formed ids) and returns it
// with gateway ids and external ids in lower case, the form the rollup stores.
func NormaliseReportSensors(in []ReportSensor, validID func(string) bool) ([]ReportSensor, error) {
	if len(in) == 0 {
		return nil, Because(ErrInvalid, "no_sensors")
	}
	if len(in) > ReportMaxSensors {
		return nil, Because(ErrInvalid, "too_many_sensors")
	}
	out := make([]ReportSensor, 0, len(in))
	seen := map[ReportSensor]bool{}
	for _, s := range in {
		n := ReportSensor{GatewayID: strings.ToLower(s.GatewayID), ExternalID: strings.ToLower(strings.TrimSpace(s.ExternalID))}
		if !validID(n.GatewayID) || n.ExternalID == "" || len(n.ExternalID) > 128 || strings.ContainsFunc(n.ExternalID, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return nil, Because(ErrInvalid, "invalid_sensor")
		}
		if seen[n] {
			return nil, Because(ErrInvalid, "duplicate_sensor")
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// ValidReportRange checks an allowed range: finite ends inside what the metric can be, min not above max.
func ValidReportRange(min, max *float64, lo, hi float64) error {
	for _, v := range []*float64{min, max} {
		if v != nil && !(*v >= lo && *v <= hi) { // also false for NaN
			return Because(ErrInvalid, "invalid_limits")
		}
	}
	if min != nil && max != nil && *min > *max {
		return Because(ErrInvalid, "invalid_limits")
	}
	return nil
}

// Bounds of an allowed range: temperature in °C, relative humidity in %.
const (
	ReportTempLow, ReportTempHigh = -100.0, 200.0
	ReportHumLow, ReportHumHigh   = 0.0, 100.0
)

// ReportCandidate says whether a profile can carry a temperature or humidity, so the sensor picker lists it: an
// environment profile, a Tuya or Zigbee2MQTT device (whose readings may), or a profile the catalog does not know.
func ReportCandidate(profileID string) bool {
	p := DeviceProfileByID(profileID)
	if p == nil {
		return true
	}
	return slices.ContainsFunc(p.Kinds, func(k string) bool { return k == "environment" || k == "tuya" || k == "zigbee" })
}
