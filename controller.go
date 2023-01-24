package main

import (
	"log"
	"math"
	"sync"
)

type strategy int

const (
	strategyUnknown strategy = iota
	strategyAuto
	strategyFullSpeed
)

type chargeMode int

const (
	chargeModeUnknown chargeMode = iota
	chargeModeEco                // Charge based on eco power limit
	chargeModeFast               // Full speed
)

type observedValues int

const (
	observedBattery observedValues = 1 << iota
	observedSolar
	observedLR
	observedChargeMode
	observedStrategy
	observedTemp
)

const (
	maxAmps       = 40
	minAmps       = 8
	volts         = 240
	maxTemp       = 450     // 45°C
	minSitePowerW = -100000 // 100kW. Don't expect single home to pull more than this from the grid.
)

type controller struct {
	lock       sync.Mutex
	cond       *sync.Cond
	seenValues observedValues

	// Sensors
	evBatteryLevelPercent float64 // 0.0 - 100.0
	exportedSolarW        float64
	tempDeciCelsius       int64
	loadReductionEnabled  bool
	controllerStrategy    strategy
	currentChargeMode     chargeMode
	setEcoPowerLimit      func(float64) error
	setChargeMode         func(chargeMode) error
}

func NewController(
	setEcoPowerLimit func(float64) error,
	setChargeMode func(chargeMode) error,
) *controller {
	cont := &controller{
		setEcoPowerLimit: setEcoPowerLimit,
		setChargeMode:    setChargeMode,
	}

	cont.cond = sync.NewCond(&cont.lock)
	return cont
}

func updateSensor[T comparable](c *controller, oldValue *T, newValue T, obs observedValues) {
	c.lock.Lock()
	defer c.lock.Unlock()

	var shouldNotify bool
	if *oldValue != newValue {
		*oldValue = newValue
		shouldNotify = true
	}
	c.seenValues |= obs
	if shouldNotify {
		c.cond.Signal()
	}
}

func (c *controller) SetEVBatteryLevelPercent(batt float64) {
	updateSensor(c, &c.evBatteryLevelPercent, batt, observedBattery)
}

func (c *controller) SetExportedSolarW(solarW float64) {
	updateSensor(c, &c.exportedSolarW, solarW, observedSolar)
}

func (c *controller) SetLoadReduction(enabled bool) {
	updateSensor(c, &c.loadReductionEnabled, enabled, observedLR)
}

func (c *controller) SetCurrentChargeMode(mode chargeMode) {
	updateSensor(c, &c.currentChargeMode, mode, observedChargeMode)
}

func (c *controller) SetControllerStrategy(strategy strategy) {
	updateSensor(c, &c.controllerStrategy, strategy, observedStrategy)
}

func (c *controller) SetEVSETempDeciCelsius(temp int64) {
	updateSensor(c, &c.tempDeciCelsius, temp, observedTemp)
}

func (c *controller) seen(checks ...observedValues) bool {
	for _, check := range checks {
		if c.seenValues&check != check {
			return false
		}
	}

	return true
}

func (c *controller) computeMaxPower() (int32, string) {
	if !c.seen(observedStrategy, observedChargeMode) {
		// Not enough data to make informed choices - try again when we have more data.
		return math.MinInt32, "not enough data"
	}

	var maxPower int32 = math.MaxInt32
	var reason string

	if c.seen(observedTemp) && c.tempDeciCelsius > maxTemp {
		maxPower = volts * minAmps
		reason = "temp exceeded limit"
	}

	if c.controllerStrategy == strategyFullSpeed {
		if reason == "" {
			reason = "manual override"
		}
		return maxPower, reason
	}

	// Load reduction is fairly high priority - it usually means bad weather (heatwave or storm).
	// Don't try to charge during this time.
	// It is up to the operator to manually charge at full speed before we're in bad weather.
	if c.seen(observedLR) && c.loadReductionEnabled {
		return 0, "load reduction enabled"
	}

	if c.seen(observedBattery) {
		if c.evBatteryLevelPercent < 60 {
			if reason == "" {
				reason = "charge level too low"
			}
			return maxPower, reason
		}
	}

	if c.seen(observedSolar) {
		if maxPower < int32(c.exportedSolarW) {
			return maxPower, reason
		}

		return int32(c.exportedSolarW), "charge level high enough"
	}

	return maxPower, reason
}

func (c *controller) singleLoop() error {
	c.lock.Lock()
	c.cond.Wait()

	defer c.lock.Unlock()

	maxPower, reason := c.computeMaxPower()

	if maxPower < minSitePowerW {
		// Not enough data. Don't take action
		return nil
	}

	if maxPower > volts*maxAmps {
		if c.currentChargeMode != chargeModeFast {
			log.Printf("Switching to Fast mode - %s", reason)
			return c.setChargeMode(chargeModeFast)
		}

		return nil
	}

	if c.currentChargeMode != chargeModeEco {
		log.Printf("Switching to Eco mode - %s", reason)
		if err := c.setChargeMode(chargeModeEco); err != nil {
			return err
		}
	}

	return c.setEcoPowerLimit(float64(maxPower))
}

func (c *controller) Loop() error {
	for {
		if err := c.singleLoop(); err != nil {
			return err
		}
	}
}
