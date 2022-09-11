package main

import (
	"log"
	"sync"
)

type strategy int

const (
	strategyAuto strategy = iota
	strategyMaxSpeed
)

type chargeMode int

const (
	chargeModeEco  chargeMode = iota // Charge based on eco power limit
	chargeModeFast                   // Full speed
)

type observedValues int

const (
	observedBattery observedValues = 1 << iota
	observedSolar
	observedLR
	observedChargeMode
)

type controller struct {
	lock       sync.Mutex
	cond       *sync.Cond
	seenValues observedValues

	// Sensors
	evBatteryLevelPercent float64 // 0.0 - 100.0
	exportedSolarKW       float64
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

func (c *controller) SetExportedSolarKW(solarKW float64) {
	updateSensor(c, &c.exportedSolarKW, solarKW, observedSolar)
}

func (c *controller) SetLoadReduction(enabled bool) {
	updateSensor(c, &c.loadReductionEnabled, enabled, observedLR)
}

func (c *controller) SetCurrentChargeMode(mode chargeMode) {
	updateSensor(c, &c.currentChargeMode, mode, observedChargeMode)
}

func (c *controller) seen(checks ...observedValues) bool {
	for _, check := range checks {
		if c.seenValues&check != check {
			return false
		}
	}

	return true
}

func (c *controller) singleLoop() error {
	c.lock.Lock()
	c.cond.Wait()

	defer c.lock.Unlock()

	// Load reduction is fairly high priority - it usually means bad weather (heatwave or storm).
	// Don't try to charge during this time.
	// It is up to the operator to manually charge at full speed before we're in bad weather.
	if c.seen(observedLR) && c.loadReductionEnabled {
		log.Println("Load reduction enabled - setting power limit to 0")
		if err := c.setEcoPowerLimit(0); err != nil {
			return err
		}
		if c.seen(observedChargeMode) && c.currentChargeMode != chargeModeEco {
			log.Println("Load reduction enabled - forcing Eco mode")
			if err := c.setChargeMode(chargeModeEco); err != nil {
				return err
			}
		}

		return nil
	}

	if c.seen(observedChargeMode, observedBattery) {
		if c.evBatteryLevelPercent < 50 && c.currentChargeMode != chargeModeFast {
			log.Println("Charge level too low - disabling Eco mode")
			if err := c.setChargeMode(chargeModeFast); err != nil {
				return err
			}
		}

		if c.evBatteryLevelPercent > 60 && c.currentChargeMode != chargeModeEco {
			log.Println("Charge level high enough - enabling Eco mode")
			if err := c.setChargeMode(chargeModeEco); err != nil {
				return err
			}
		}
	}

	if c.seen(observedSolar) {
		if err := c.setEcoPowerLimit(c.exportedSolarKW); err != nil {
			return err
		}
	}

	return nil
}

func (c *controller) Loop() error {
	for {
		if err := c.singleLoop(); err != nil {
			return err
		}
	}
}
