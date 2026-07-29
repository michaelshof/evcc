package charger

// LICENSE

// Copyright (c) evcc.io (andig, naltatis, premultiply)

// This module is NOT covered by the MIT license. All rights reserved.

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
	"github.com/evcc-io/evcc/util/sponsor"
	"github.com/volkszaehler/mbmd/encoding"
)

// Hager Witty Plus ModbusTCP developer's Guide v1.0.0 (June 2025)

// HagerWittyPlus charger implementation
type HagerWittyPlus struct {
	conn    *modbus.Connection
	current uint16 // A*10
}

const (
	hagerRegModbusVersion      = 0x1003 // unit16
	hagerRegProductRef         = 0x1003 // 16 regs, string
	hagerRegUniqueID           = 0x1013 // 16 regs, string
	hagerRegSoftware           = 0x1023 // 3 regs
	hagerRegMacAddress         = 0x2000 // 3 regs, 6 hex bytes
	hagerRegIpAddress          = 0x200F // 2 regs, ip hex
	hagerRegTimestamp          = 0x3000 // 4 regs, uint64 UTC seconds, R/W
	hagerRegTimestampIsSet     = 0x3005 // enum, 0=NOT-set, 1=IS-set
	hagerRegHardwareMaxCurrent = 0x3006 // uint16, A
	hagerRegCableMaxCurrent    = 0x3007 // uint16, A
	hagerRegCPState            = 0x3008 // 1 reg, ASCII A1..F
	hagerRegPwm                = 0x3009 // uint16, 0-100%
	hagerRegCurrents           = 0x300A // 3x int16, A/10
	hagerRegVoltages           = 0x300D // 3x uint16, V/10
	hagerRegPowers             = 0x3010 // 3x int16, W
	hagerRegPower              = 0x3013 // int32, W
	hagerRegMaxCurrent         = 0x301E // uint16, A/10, R/W
	hagerRegMaxCurrentFB       = 0x301F // uint16, A/10, fallback R/W
	hagerRegAvailability       = 0x3020 // enum, 0=not-available, 1=available, R/W
	hagerRegAvailabilityFB     = 0x3021 // enum, 0=not-available, 1=available, R/W
	hagerRegPhases             = 0x3022 // enum, 0=3p, 1=1p, R/W
	hagerRegPhasesFB           = 0x3023 // enum, 0=3p, 1=1p, R/W
	hagerRegLockActuatorState  = 0x3025 // enum, 0=unlocked, 1=locked, 2=disable, 3=blocked-unlocked, 4=blocked-locked
	hagerRegFallback           = 0x3026 // enum, 0=immediate, 1=delayed, R/W
	hagerRegWallboxStatus      = 0x302B // 2 regs, enum
	hagerRegRfidSize           = 0x4001 // unit16, values=4,7,10
	hagerRegRfidUID            = 0x4002 // 5 regs, byte array
	hagerRegRfidFeedback       = 0x400B // enum, 0=no-feedback, 1=authorized, 2=not-authorized, 3=discarded, R/W
	hagerRegSessionEnergy      = 0x5000 // uint32, Wh
	hagerRegSessionStart       = 0x5002 // 4 regs, uint64 UTC seconds
	hagerRegSessionStop        = 0x5006 // 4 regs, uint64 UTC seconds
	hagerRegSessionDuration    = 0x500A // 2 regs, uint32 UTC seconds
	hagerRegSessionRfidSize    = 0x500C // uint16, values=4,7,10
	hagerRegSessionRfidUID     = 0x500D // 5 regs, byte array
	hagerRegSessionRfidType    = 0x5012 // enum, 0=local-badge,1=remote-badge
	hagerRegSessionStopReason  = 0x5013 // enum, 1 reg, enum
	hagerRegSessionSate        = 0x5014 // enum, 0=no-session, 1=session-running, 2=session-ended

	hagerInvalidCurrent   = 0x7FFF
	hagerEnableMaxCurrent = 60 // A*10 = 60
)

func init() {
	registry.AddCtx("hager-witty-plus-modbus", NewHagerWittyPlusFromConfig)
}

// NewHagerWittyPlusFromConfig creates a Hager Witty Plus charger from generic config
func NewHagerWittyPlusFromConfig(ctx context.Context, other map[string]any) (api.Charger, error) {
	cc := modbus.TcpSettings{
		ID: 1,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	return NewHagerWittyPlus(ctx, cc)
}

// NewHagerWittyPlus creates a Hager Witty Plus charger
func NewHagerWittyPlus(ctx context.Context, settings modbus.TcpSettings) (*HagerWittyPlus, error) {
	conn, err := settings.Connection(ctx)
	if err != nil {
		return nil, err
	}

	if !sponsor.IsAuthorized() {
		return nil, api.ErrSponsorRequired
	}

	log := util.NewLogger("hager")
	conn.Logger(log.TRACE)

	wb := &HagerWittyPlus{
		conn:    conn,
		current: hagerEnableMaxCurrent,
	}

	// failsafe: stop charging if Modbus connection is lost
	if _, err := wb.conn.WriteSingleRegister(hagerRegMaxCurrentFB, 0); err != nil {
		return nil, fmt.Errorf("fallback current: %w", err)
	}

	if err := wb.setAvailable(true); err != nil {
		return nil, fmt.Errorf("availability: %w", err)
	}

	if b, err := wb.conn.ReadHoldingRegisters(hagerRegMaxCurrent, 1); err == nil {
		if u := binary.BigEndian.Uint16(b); u >= 60 {
			wb.current = u
		}
	}

	return wb, nil
}

func (wb *HagerWittyPlus) setAvailable(available bool) error {
	var u uint16
	if available {
		u = 1
	}

	_, err := wb.conn.WriteSingleRegister(hagerRegAvailability, u)
	return err
}

// Status implements the api.Charger interface
func (wb *HagerWittyPlus) Status() (api.ChargeStatus, error) {
	b, err := wb.conn.ReadHoldingRegisters(hagerRegCPState, 1)
	if err != nil {
		return api.StatusNone, err
	}

	return api.ChargeStatusStringWithMapping(string(b), api.StatusEasA)
}

// Enabled implements the api.Charger interface
func (wb *HagerWittyPlus) Enabled() (bool, error) {
	b, err := wb.conn.ReadHoldingRegisters(hagerRegMaxCurrent, 1)
	if err != nil {
		return false, err
	}

	return binary.BigEndian.Uint16(b) != 0, nil
}

// Enable implements the api.Charger interface
func (wb *HagerWittyPlus) Enable(enable bool) error {
	var u uint16
	if enable {
		if err := wb.setAvailable(true); err != nil {
			return err
		}
		if err := wb.setTimestamp(time.Now()); err != nil {
			return err
		}
		u = wb.current
	}

	_, err := wb.conn.WriteSingleRegister(hagerRegMaxCurrent, u)
	return err
}

func (wb *HagerWittyPlus) setTimestamp(t time.Time) error {
	// wallbox requires a valid UTC timestamp before a charging session can start
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(t.Unix()))
	_, err := wb.conn.WriteMultipleRegisters(hagerRegTimestamp, 4, b)
	return err
}

// MaxCurrent implements the api.Charger interface
func (wb *HagerWittyPlus) MaxCurrent(current int64) error {
	return wb.MaxCurrentMillis(float64(current))
}

var _ api.ChargerEx = (*HagerWittyPlus)(nil)

// MaxCurrentMillis implements the api.ChargerEx interface
func (wb *HagerWittyPlus) MaxCurrentMillis(current float64) error {
	if current < 6 {
		return fmt.Errorf("invalid current %.1f", current)
	}

	curr := uint16(current * 10)

	_, err := wb.conn.WriteSingleRegister(hagerRegMaxCurrent, curr)
	if err == nil {
		wb.current = curr
	}

	return err
}

var _ api.Meter = (*HagerWittyPlus)(nil)

// CurrentPower implements the api.Meter interface
func (wb *HagerWittyPlus) CurrentPower() (float64, error) {
	b, err := wb.conn.ReadHoldingRegisters(hagerRegPower, 2)
	if err != nil {
		return 0, err
	}

	return float64(encoding.Int32(b)), nil
}

var _ api.ChargeRater = (*HagerWittyPlus)(nil)

// ChargedEnergy implements the api.ChargeRater interface
func (wb *HagerWittyPlus) ChargedEnergy() (float64, error) {
	b, err := wb.conn.ReadHoldingRegisters(hagerRegSessionEnergy, 2)
	if err != nil {
		return 0, err
	}

	return float64(encoding.Uint32(b)) / 1e3, nil
}

var _ api.PhaseCurrents = (*HagerWittyPlus)(nil)

// Currents implements the api.PhaseCurrents interface
func (wb *HagerWittyPlus) Currents() (float64, float64, float64, error) {
	b, err := wb.conn.ReadHoldingRegisters(hagerRegCurrents, 3)
	if err != nil {
		return 0, 0, 0, err
	}

	var res [3]float64
	for i := range res {
		u := binary.BigEndian.Uint16(b[2*i:])
		if u == hagerInvalidCurrent {
			continue
		}
		res[i] = float64(int16(u)) / 10
	}

	return res[0], res[1], res[2], nil
}

var _ api.PhaseVoltages = (*HagerWittyPlus)(nil)

// Voltages implements the api.PhaseVoltages interface
func (wb *HagerWittyPlus) Voltages() (float64, float64, float64, error) {
	b, err := wb.conn.ReadHoldingRegisters(hagerRegVoltages, 3)
	if err != nil {
		return 0, 0, 0, err
	}

	var res [3]float64
	for i := range res {
		res[i] = float64(binary.BigEndian.Uint16(b[2*i:])) / 10
	}

	return res[0], res[1], res[2], nil
}

var _ api.PhasePowers = (*HagerWittyPlus)(nil)

// Powers implements the api.PhasePowers interface
func (wb *HagerWittyPlus) Powers() (float64, float64, float64, error) {
	b, err := wb.conn.ReadHoldingRegisters(hagerRegPowers, 3)
	if err != nil {
		return 0, 0, 0, err
	}

	var res [3]float64
	for i := range res {
		res[i] = float64(int16(binary.BigEndian.Uint16(b[2*i:])))
	}

	return res[0], res[1], res[2], nil
}

var _ api.PhaseSwitcher = (*HagerWittyPlus)(nil)

// Phases1p3p implements the api.PhaseSwitcher interface
func (wb *HagerWittyPlus) Phases1p3p(phases int) error {
	var u uint16
	switch phases {
	case 1:
		u = 1
	case 3:
		u = 0
	default:
		return fmt.Errorf("invalid phases: %d", phases)
	}

	_, err := wb.conn.WriteSingleRegister(hagerRegPhases, u)
	return err
}

var _ api.PhaseGetter = (*HagerWittyPlus)(nil)

// GetPhases implements the api.PhaseGetter interface
func (wb *HagerWittyPlus) GetPhases() (int, error) {
	b, err := wb.conn.ReadHoldingRegisters(hagerRegPhases, 1)
	if err != nil {
		return 0, err
	}

	if binary.BigEndian.Uint16(b) == 1 {
		return 1, nil
	}

	return 3, nil
}

var _ api.Identifier = (*HagerWittyPlus)(nil)

// Identify implements the api.Identifier interface
func (wb *HagerWittyPlus) Identify() (string, error) {
	sizeReg, err := wb.conn.ReadHoldingRegisters(hagerRegRfidSize, 1)
	if err != nil {
		return "", err
	}

	size := int(binary.BigEndian.Uint16(sizeReg))
	if size != 4 && size != 7 && size != 10 {
		return "", nil
	}

	b, err := wb.conn.ReadHoldingRegisters(hagerRegRfidUID, 5)
	if err != nil {
		return "", err
	}

	if size > len(b) {
		size = len(b)
	}

	uid := b[:size]
	if allZero(uid) {
		return "", nil
	}

	return strings.ToUpper(hex.EncodeToString(uid)), nil
}

var _ api.Diagnosis = (*HagerWittyPlus)(nil)

// Diagnose implements the api.Diagnosis interface
func (wb *HagerWittyPlus) Diagnose() {
	if b, err := wb.conn.ReadHoldingRegisters(hagerRegProductRef, 16); err == nil {
		fmt.Printf("\tProduct:\t%s\n", trimModbusString(b))
	}
	if b, err := wb.conn.ReadHoldingRegisters(hagerRegUniqueID, 16); err == nil {
		fmt.Printf("\tUnique ID:\t%s\n", trimModbusString(b))
	}
	if b, err := wb.conn.ReadHoldingRegisters(hagerRegSoftware, 3); err == nil {
		major := b[0]
		minor := b[1]
		revision := binary.BigEndian.Uint16(b[2:4])
		build := binary.BigEndian.Uint16(b[4:6])
		fmt.Printf("\tSoftware:\t%d.%d.%d.%d\n", major, minor, revision, build)
	}
	if b, err := wb.conn.ReadHoldingRegisters(hagerRegMaxCurrent, 1); err == nil {
		fmt.Printf("\tMax current:\t%.1fA\n", float64(binary.BigEndian.Uint16(b))/10)
	}
	if b, err := wb.conn.ReadHoldingRegisters(hagerRegAvailability, 1); err == nil {
		fmt.Printf("\tAvailability:\t%d\n", binary.BigEndian.Uint16(b))
	}
	if b, err := wb.conn.ReadHoldingRegisters(hagerRegCPState, 1); err == nil {
		fmt.Printf("\tCP state:\t%s\n", string(b))
	}
	if b, err := wb.conn.ReadHoldingRegisters(hagerRegWallboxStatus, 2); err == nil {
		fmt.Printf("\tWallbox status:\t0x%08X\n", encoding.Uint32(b))
	}
}
