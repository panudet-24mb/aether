package simulation

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// The digital twin demo (cmd/demo-twin, docs/platform/digital-twin.md): a fictional three-floor hospital with
// 14 zones, 10 MG3-style gateways, 60 sensors and 40 people wearing B10 wristbands, and a 30-minute scripted
// loop that exercises everything the twin draws. Everything here is deterministic (a function of the step),
// so `run` and `trigger` agree on where everybody is without talking to each other, and tests can pin it.
//
// All MACs start with f1, outside any real vendor range; every row carries aether_source "simulated".

const (
	// TwinStepSec is one uplink per gateway: 10 gateways every 6 s is 100 requests a minute, under the ingest
	// limit of 120 per minute for one client.
	TwinStepSec = 6
	// TwinLoopSteps is the 30-minute scripted loop.
	TwinLoopSteps = 30 * 60 / TwinStepSec
)

// Scripted moments of the loop, in steps.
const (
	TwinWanderFrom  = 8 * 60 / TwinStepSec  // minute 8: a patient walks into the restricted server room
	TwinWanderTo    = 11 * 60 / TwinStepSec // and is walked back three minutes later
	TwinSOSStep     = 12 * 60 / TwinStepSec // minute 12: SOS in ward 3
	TwinDoorFrom    = 18 * 60 / TwinStepSec // minute 18: someone at the cold-store door (PIR) for three minutes
	TwinDoorTo      = 21 * 60 / TwinStepSec
	TwinSpikeFrom   = 20 * 60 / TwinStepSec // minute 20: the cold store warms to 12 °C
	TwinSpikePeak   = 23 * 60 / TwinStepSec
	TwinSpikeTo     = 26 * 60 / TwinStepSec // minute 26: back to normal
	TwinColdBaseC   = 5.0
	TwinColdSpikeC  = 12.0
	TwinColdAlertAt = 8.0 // the demo's template threshold
)

type TwinRect [4]float64 // x0, y0, x1, y1 in metres

func (r TwinRect) Points() [][2]float64 {
	return [][2]float64{{r[0], r[1]}, {r[2], r[1]}, {r[2], r[3]}, {r[0], r[3]}}
}
func (r TwinRect) Center() (float64, float64) { return (r[0] + r[2]) / 2, (r[1] + r[3]) / 2 }

type TwinZone struct {
	Name, Kind, Color string
	Rect              TwinRect
	Gateway           int // index into Gateways, -1 when no gateway covers the zone
	// Comfort is the zone's temperature band when it differs from its kind's default (the cold store: 2–8 °C).
	Comfort *[2]float64
}

type TwinFloor struct {
	Name          string
	Level         int
	Width, Depth  float64
	Zones         []TwinZone
	Beds, Desks   [][2]float64
	Doors         [][3]float64 // x, y, rotation
	GatewaysOnMe  []int
	ColdStoreZone int // -1 unless this floor holds the cold store
}

type TwinGateway struct {
	Name        string
	MAC         string
	Floor, Zone int
	X, Y        float64
}

type TwinSensor struct {
	Name         string
	MAC          string
	Kind         string // environment | pir
	Floor, Zone  int
	Gateway      int // the gateway it is registered on and heard by
	X, Y, Z      float64
	BaseC, BaseH float64
	Cold         bool // in the cold store: follows the spike
	ColdDoor     bool // the PIR at the cold-store door
}

type TwinWearer struct {
	Name string
	Role string
	MAC  string
	// Route is the cycle of gateways the wearer walks, each held for Dwell steps; Offset desynchronises people.
	Route  []int
	Dwell  int
	Offset int
	// Wander sends this wearer to the restricted room between TwinWanderFrom and TwinWanderTo.
	Wander bool
	// SOS makes this wearer press the button at TwinSOSStep.
	SOS bool
}

type TwinBuilding struct {
	Floors   []TwinFloor
	Gateways []TwinGateway
	Sensors  []TwinSensor
	People   []TwinWearer
	// Indices of the scripted places.
	ColdStoreGateway, RestrictedGateway, Ward3Gateway int
}

// TwinDemo builds the fictional hospital. The same call always returns the same building.
func TwinDemo() TwinBuilding {
	b := TwinBuilding{}
	floor := func(name string, level int, zones []TwinZone) int {
		b.Floors = append(b.Floors, TwinFloor{Name: name, Level: level, Width: 40, Depth: 24, Zones: zones, ColdStoreZone: -1})
		return len(b.Floors) - 1
	}
	f1 := floor("ชั้น 1 · ฉุกเฉินและเภสัชกรรม", 1, []TwinZone{
		{Name: "โถงต้อนรับ", Kind: "room", Color: "mint", Rect: TwinRect{0, 0, 14, 10}},
		{Name: "ห้องฉุกเฉิน ER", Kind: "ward", Color: "coral", Rect: TwinRect{14, 0, 28, 10}},
		{Name: "ห้องยา", Kind: "storage", Color: "violet", Rect: TwinRect{28, 0, 40, 10}},
		{Name: "ทางเดินชั้น 1", Kind: "corridor", Color: "slate", Rect: TwinRect{0, 10, 28, 14}},
		{Name: "ห้องแล็บ", Kind: "room", Color: "blue", Rect: TwinRect{0, 14, 28, 24}},
		{Name: "ห้องเย็นเก็บยาและวัคซีน", Kind: "storage", Color: "blue", Rect: TwinRect{28, 10, 40, 24}, Comfort: &[2]float64{2, 8}},
	})
	b.Floors[f1].ColdStoreZone = 5
	f2 := floor("ชั้น 2 · หอผู้ป่วยใน", 2, []TwinZone{
		{Name: "หอผู้ป่วย 1", Kind: "ward", Color: "mint", Rect: TwinRect{0, 0, 13, 10}},
		{Name: "หอผู้ป่วย 2", Kind: "ward", Color: "mint", Rect: TwinRect{13, 0, 26, 10}},
		{Name: "หอผู้ป่วย 3", Kind: "ward", Color: "mint", Rect: TwinRect{26, 0, 40, 10}},
		{Name: "ทางเดินชั้น 2", Kind: "corridor", Color: "slate", Rect: TwinRect{0, 10, 40, 14}},
		{Name: "เคาน์เตอร์พยาบาล", Kind: "room", Color: "amber", Rect: TwinRect{14, 14, 26, 24}},
	})
	f3 := floor("ชั้น 3 · ICU", 3, []TwinZone{
		{Name: "ICU A", Kind: "ward", Color: "blue", Rect: TwinRect{0, 0, 20, 12}},
		{Name: "ICU B", Kind: "ward", Color: "blue", Rect: TwinRect{20, 0, 40, 12}},
		{Name: "ห้องเซิร์ฟเวอร์ (หวงห้าม)", Kind: "restricted", Color: "coral", Rect: TwinRect{28, 14, 40, 24}},
	})

	gateway := func(f, z int, name string) int {
		x, y := b.Floors[f].Zones[z].Rect.Center()
		i := len(b.Gateways)
		b.Gateways = append(b.Gateways, TwinGateway{Name: name, MAC: fmt.Sprintf("f1a0000000%02x", i), Floor: f, Zone: z, X: x, Y: y})
		b.Floors[f].Zones[z].Gateway = i
		b.Floors[f].GatewaysOnMe = append(b.Floors[f].GatewaysOnMe, i)
		return i
	}
	for fi := range b.Floors {
		for zi := range b.Floors[fi].Zones {
			b.Floors[fi].Zones[zi].Gateway = -1
		}
	}
	gLobby := gateway(f1, 0, "GW โถงต้อนรับ")
	gER := gateway(f1, 1, "GW ห้องฉุกเฉิน")
	gCold := gateway(f1, 5, "GW ห้องเย็น")
	gW1 := gateway(f2, 0, "GW หอผู้ป่วย 1")
	gW2 := gateway(f2, 1, "GW หอผู้ป่วย 2")
	gW3 := gateway(f2, 2, "GW หอผู้ป่วย 3")
	gNurse := gateway(f2, 4, "GW เคาน์เตอร์พยาบาล")
	gICUA := gateway(f3, 0, "GW ICU A")
	gICUB := gateway(f3, 1, "GW ICU B")
	gServer := gateway(f3, 2, "GW ห้องเซิร์ฟเวอร์")
	b.ColdStoreGateway, b.RestrictedGateway, b.Ward3Gateway = gCold, gServer, gW3
	// Zones without their own gateway: their sensors are heard by the nearest one on the floor.
	nearest := map[[2]int]int{{f1, 2}: gCold, {f1, 3}: gLobby, {f1, 4}: gLobby, {f2, 3}: gNurse}

	// Environment sensors: four in each zone with a gateway, three elsewhere, on a grid inside the zone.
	base := func(kind string, fi, zi int) (float64, float64) {
		switch {
		case fi == f1 && zi == 5:
			return TwinColdBaseC, 40
		case kind == "restricted":
			return 19.5, 38
		case kind == "corridor":
			return 27, 58
		case fi == f3:
			return 22.5, 50
		case kind == "ward":
			return 24.5, 55
		}
		return 25.5, 52
	}
	env := 0
	for fi, f := range b.Floors {
		for zi, z := range f.Zones {
			n := 3
			if z.Gateway >= 0 {
				n = 4
			}
			gw := z.Gateway
			if gw < 0 {
				gw = nearest[[2]int{fi, zi}]
			}
			c, h := base(z.Kind, fi, zi)
			w, d := z.Rect[2]-z.Rect[0], z.Rect[3]-z.Rect[1]
			for k := 0; k < n; k++ {
				fx := []float64{0.25, 0.75, 0.25, 0.75}[k]
				fy := []float64{0.28, 0.28, 0.72, 0.72}[k]
				if n == 3 {
					fx, fy = []float64{0.2, 0.5, 0.8}[k], 0.5
				}
				env++
				b.Sensors = append(b.Sensors, TwinSensor{Name: fmt.Sprintf("อุณหภูมิ %s #%d", z.Name, k+1), MAC: fmt.Sprintf("f1e00000%04x", env), Kind: "environment",
					Floor: fi, Zone: zi, Gateway: gw, X: round1(z.Rect[0] + w*fx), Y: round1(z.Rect[1] + d*fy), Z: 1.6,
					BaseC: c + 0.4*float64(k%2) - 0.2*float64(k/2), BaseH: h + float64(k), Cold: fi == f1 && zi == 5})
			}
		}
	}
	// PIR occupancy sensors, one per busy room, and one at the cold-store door.
	pir := func(fi, zi int, name string, x, y float64, door bool) {
		z := b.Floors[fi].Zones[zi]
		gw := z.Gateway
		if gw < 0 {
			gw = nearest[[2]int{fi, zi}]
		}
		b.Sensors = append(b.Sensors, TwinSensor{Name: name, MAC: fmt.Sprintf("f1d00000%04x", len(b.Sensors)), Kind: "pir", Floor: fi, Zone: zi, Gateway: gw,
			X: x, Y: y, Z: 2.6, BaseC: 25, BaseH: 50, ColdDoor: door})
	}
	pir(f1, 0, "PIR โถงต้อนรับ", 7, 5, false)
	pir(f1, 1, "PIR ห้องฉุกเฉิน", 21, 5, false)
	pir(f1, 5, "PIR ประตูห้องเย็น", 29, 12, true)
	pir(f2, 0, "PIR หอผู้ป่วย 1", 6.5, 5, false)
	pir(f2, 1, "PIR หอผู้ป่วย 2", 19.5, 5, false)
	pir(f2, 2, "PIR หอผู้ป่วย 3", 33, 5, false)
	pir(f3, 0, "PIR ICU A", 10, 6, false)
	pir(f3, 1, "PIR ICU B", 30, 6, false)

	// Furniture, so the plan reads as a hospital.
	for _, x := range []float64{2, 5, 8, 11, 15, 18, 21, 24, 28, 31, 34, 37} {
		b.Floors[f2].Beds = append(b.Floors[f2].Beds, [2]float64{x, 1.2})
	}
	for _, x := range []float64{2, 6, 10, 14, 22, 26, 30, 34} {
		b.Floors[f3].Beds = append(b.Floors[f3].Beds, [2]float64{x, 1.5})
	}
	for _, x := range []float64{16, 19, 22, 25} {
		b.Floors[f1].Beds = append(b.Floors[f1].Beds, [2]float64{x, 1.2})
	}
	b.Floors[f1].Desks = [][2]float64{{4, 6}, {2, 17}, {8, 17}, {14, 17}}
	b.Floors[f2].Desks = [][2]float64{{17, 18}, {21, 18}}
	for fi := range b.Floors {
		for _, z := range b.Floors[fi].Zones {
			if z.Kind == "corridor" {
				continue
			}
			x, _ := z.Rect.Center()
			y := z.Rect[3]
			if z.Rect[1] >= 10 {
				y = z.Rect[1]
			}
			b.Floors[fi].Doors = append(b.Floors[fi].Doors, [3]float64{x - 0.45, y, 0})
		}
	}

	// People: fictional names only.
	nurses := []string{"อรวรรณ", "สุภาพร", "กนกพร", "ปวีณา", "ศิริพร", "จันทร์เพ็ญ", "นภัสสร", "วิลาวัลย์", "พิมพ์ชนก", "ธนพร", "รัตนา", "มาลัย", "อัญชลี", "ดวงใจ", "ปิยะนุช", "สายสุนีย์"}
	doctors := []string{"ธีรวัฒน์", "ภานุพงศ์", "ณัฐวุฒิ", "ชยพล", "กิตติพัฒน์", "วรวิทย์"}
	staff := []string{"สมชาย", "ประเสริฐ", "วิชัย", "บุญมี", "สุรเชษฐ์", "อำนาจ", "ไพโรจน์", "เกรียงไกร"}
	patients := []string{"ประภา", "สมศรี", "บุญเรือน", "ทองใบ", "คำปุ่น", "สายทอง", "มณี", "จำปา", "บัวผัน", "เพ็ญศรี"}
	mac := func() string { return fmt.Sprintf("f1b00000%04x", len(b.People)+1) }
	wards := []int{gW1, gW2, gW3}
	for i, n := range nurses {
		route := []int{wards[i%3], gNurse, wards[(i+1)%3], gNurse}
		b.People = append(b.People, TwinWearer{Name: "พยาบาล" + n, Role: "nurse", MAC: mac(), Route: route, Dwell: 18 + i%5*4, Offset: i * 11})
	}
	for i, n := range doctors {
		route := []int{gICUA, gICUB, gER, gICUA}
		if i%2 == 1 {
			route = []int{gER, gLobby, gICUB, gW2}
		}
		b.People = append(b.People, TwinWearer{Name: "นพ." + n, Role: "doctor", MAC: mac(), Route: route, Dwell: 30 + i*5, Offset: i * 23})
	}
	for i, n := range staff {
		route := []int{gLobby, gER, gCold, gLobby}
		if i%2 == 1 {
			route = []int{gCold, gLobby, gServer, gLobby}
		}
		b.People = append(b.People, TwinWearer{Name: "เจ้าหน้าที่" + n, Role: "staff", MAC: mac(), Route: route, Dwell: 25 + i*3, Offset: i * 17})
	}
	for i, n := range patients {
		home := wards[i%3]
		p := TwinWearer{Name: "ผู้ป่วย คุณ" + n, Role: "patient", MAC: mac(), Route: []int{home, home, home, gNurse}, Dwell: 40, Offset: i * 29}
		switch i {
		case 0: // presses SOS in ward 3 and stays there
			p.Route, p.SOS = []int{gW3}, true
		case 1: // wanders into the server room at minute 8
			p.Route, p.Wander = []int{gW1}, true
		}
		b.People = append(b.People, p)
	}
	return b
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// TwinGatewayOf is the gateway a wearer is near at a step (the loop repeats every TwinLoopSteps).
func (b TwinBuilding) TwinGatewayOf(i, step int) int {
	p := b.People[i]
	s := ((step % TwinLoopSteps) + TwinLoopSteps) % TwinLoopSteps
	if p.Wander && s >= TwinWanderFrom && s < TwinWanderTo {
		return b.RestrictedGateway
	}
	if len(p.Route) == 1 || p.Dwell <= 0 {
		return p.Route[0]
	}
	return p.Route[((s+p.Offset)/p.Dwell)%len(p.Route)]
}

// TwinStep is the loop step at a wall-clock time: run and trigger agree on it without talking to each other.
func TwinStep(at time.Time) int { return int(at.Unix()/TwinStepSec) % TwinLoopSteps }

// TwinOverrides are on-demand events (`demo-twin trigger`), on top of the script.
type TwinOverrides struct {
	SOS        bool // the scripted SOS wearer presses now
	SpikeUntil time.Time
	DoorUntil  time.Time
	// Warmup leaves out the faint second gateway: the first time a tag is heard decides its zone at once, so the
	// first uplink must come only from the gateway it is really near (after that the ingest's hysteresis holds).
	Warmup bool
}

// TwinColdC is the cold-store temperature at a step: flat, then the scripted (or triggered) warming.
func TwinColdC(step int, spike bool) float64 {
	s := ((step % TwinLoopSteps) + TwinLoopSteps) % TwinLoopSteps
	switch {
	case spike:
		return TwinColdSpikeC
	case s >= TwinSpikeFrom && s < TwinSpikePeak:
		return TwinColdBaseC + (TwinColdSpikeC-TwinColdBaseC)*float64(s-TwinSpikeFrom)/float64(TwinSpikePeak-TwinSpikeFrom)
	case s >= TwinSpikePeak && s < TwinSpikeTo:
		return TwinColdSpikeC - (TwinColdSpikeC-TwinColdBaseC)*float64(s-TwinSpikePeak)/float64(TwinSpikeTo-TwinSpikePeak)
	}
	return TwinColdBaseC
}

// TwinPackets returns one MG3-style JSON-LONG uplink per gateway (by gateway index) for a step.
func (b TwinBuilding) TwinPackets(step int, at time.Time, o TwinOverrides) map[int][]byte {
	ts := at.UnixMilli()
	s := ((step % TwinLoopSteps) + TwinLoopSteps) % TwinLoopSteps
	rows := map[int][]map[string]any{}
	row := func(mac, raw string, rssi int) map[string]any {
		return map[string]any{"mac": mac, "rawData": raw, "rssi": rssi, "timestamp": ts, "aether_source": "simulated"}
	}
	for gi, g := range b.Gateways {
		rows[gi] = []map[string]any{{"type": "Gateway", "mac": g.MAC, "timestamp": ts, "aether_source": "simulated"}}
	}
	spike := at.Before(o.SpikeUntil)
	door := at.Before(o.DoorUntil) || (s >= TwinDoorFrom && s < TwinDoorTo)
	for si, sn := range b.Sensors {
		rssi := -50 - si%9*3
		battery := 97 - si%23
		switch sn.Kind {
		case "environment":
			c := sn.BaseC + 0.35*math.Sin(float64(s)/19+float64(si))
			if sn.Cold {
				c = TwinColdC(s, spike) + 0.2*math.Sin(float64(s)/7+float64(si))
			}
			h := sn.BaseH + 1.5*math.Cos(float64(s)/23+float64(si))
			rows[sn.Gateway] = append(rows[sn.Gateway], row(sn.MAC, thFrame(sn.MAC, battery, c, h), rssi))
			if (s+si)%20 == 0 {
				rows[sn.Gateway] = append(rows[sn.Gateway], row(sn.MAC, infoFrame(sn.MAC, battery, "S1"), rssi))
			}
		case "pir":
			// Busy rooms see somebody every so often; the cold-store door PIR fires only for the scripted visit.
			motion := !sn.ColdDoor && (s+si*7)%9 < 2
			if sn.ColdDoor {
				motion = door && s%2 == 0
			}
			rows[sn.Gateway] = append(rows[sn.Gateway], row(sn.MAC, pirFrame(sn.MAC, battery, motion), rssi))
			if (s+si)%20 == 0 {
				rows[sn.Gateway] = append(rows[sn.Gateway], row(sn.MAC, infoFrame(sn.MAC, battery, "MSP01"), rssi))
			}
		}
	}
	for pi, p := range b.People {
		gw := b.TwinGatewayOf(pi, s)
		wobble := int(math.Round(4 * math.Sin(float64(s)*1.3+float64(pi))))
		heard := [][2]int{{gw, -54 + wobble}}
		// The next gateway on the same floor hears the tag faintly, so presence has something to weigh.
		fl := b.Gateways[gw].Floor
		for _, other := range b.Floors[fl].GatewaysOnMe {
			if other != gw && !o.Warmup {
				heard = append(heard, [2]int{other, -84 - (pi+other)%5})
				break
			}
		}
		pressing := p.SOS && (s == TwinSOSStep || s == TwinSOSStep+1 || o.SOS)
		for hi, h := range heard {
			rows[h[0]] = append(rows[h[0]], row(p.MAC, accelFrame(p.MAC, 82, 0.1*math.Sin(float64(s)/2+float64(pi)), 0.08*math.Cos(float64(s)/3), 0.97), h[1]))
			if (s+pi)%15 == 0 {
				rows[h[0]] = append(rows[h[0]], row(p.MAC, infoFrame(p.MAC, 82, "B10"), h[1]))
			}
			// The B10 advertises its iBeacon slot only after its button is pressed (ButtonTriggerQuiet). Only the
			// gateway the wearer is near catches the short burst, so the alert names the right room.
			if pressing && hi == 0 {
				rows[h[0]] = append(rows[h[0]], row(p.MAC, iBeaconFrame(7, pi+1, -59), h[1]))
			}
		}
	}
	out := map[int][]byte{}
	for gi, r := range rows {
		out[gi], _ = json.Marshal(r)
	}
	return out
}
