package display

import (
	"fmt"
	"os"
	"pkg.deepin.io/dde/api/drandr"
)

func (dpy *Manager) ListOutputNames() []string {
	return dpy.outputInfos.ListNames()
}

func (dpy *Manager) ListOutputsCommonModes() (drandr.ModeInfos, error) {
	connected, err := dpy.multiOutputCheck()
	if err != nil {
		return nil, err
	}

	return connected.FoundCommonModes(), nil
}

func (dpy *Manager) SetPrimary(primary string) error {
	if dpy.Primary == primary {
		return nil
	}

	m := dpy.Monitors.getByName(primary)
	if m == nil {
		return fmt.Errorf("Invalid monitor name: %s", primary)
	}
	var cmd = fmt.Sprintf("xrandr %s", m.generateCommandline(primary, false))
	err := doAction(cmd)
	if err != nil {
		logger.Warningf("[SetPrimary] do action(%s) failed: %v", cmd, err)
		return err
	}
	dpy.setPropPrimary(primary)
	dpy.setPropHasChanged(true)
	return nil
}

func (dpy *Manager) SetBrightness(name string, value float64) error {
	err := dpy.doSetBrightness(value, name)
	if err != nil {
		logger.Warning("Failed to set brightness:", name, value, err)
		return err
	}
	dpy.SaveBrightness()
	return nil
}

func (dpy *Manager) SwitchMode(mode uint8, name string) error {
	logger.Debug("[SwitchMode] will to mode:", mode, name)
	var err error
	switch mode {
	case DisplayModeMirror:
		err = dpy.switchToMirror()
	case DisplayModeExtend:
		err = dpy.switchToExtend()
	case DisplayModeOnlyOne:
		err = dpy.switchToOnlyOne(name)
	case DisplayModeCustom:
		if name == "" {
			logger.Warning("Must input custom mode name")
			return fmt.Errorf("Empty custom mode name")
		}
		err = dpy.switchToCustom(name)
	default:
		logger.Warning("Invalid display mode:", mode)
		return fmt.Errorf("Invalid display mode: %d", mode)
	}
	if err != nil {
		logger.Errorf("Switch mode to %d failed: %v", mode, err)
		return err
	}

	dpy.setPropDisplayMode(mode)
	dpy.setPropHasChanged(false)
	if dpy.Primary != dpy.setting.GetString(gsKeyPrimary) {
		dpy.setting.SetString(gsKeyPrimary, dpy.Primary)
	}
	return dpy.Save()
}

func (dpy *Manager) ApplyChanges() error {
	monitorsLocker.Lock()
	defer monitorsLocker.Unlock()
	if !dpy.HasChanged {
		return nil
	}

	// err := dpy.doSetPrimary(dpy.Primary, true)
	// if err != nil {
	// 	logger.Error("Set primary failed:", dpy.Primary, err)
	// 	err = dpy.trySetPrimary(true)
	// 	if err != nil {
	// 		logger.Error("Try set primary failed:", err)
	// 		return err
	// 	}
	// }
	err := dpy.doApply(dpy.Primary, false)
	if err != nil {
		logger.Error("Apply changes failed:", err)
		return err
	}
	dpy.rotateInputPointor()
	return nil
}

func (dpy *Manager) ResetChanges() error {
	if !dpy.HasChanged {
		return nil
	}

	// firstly to find the matched config,
	// then update monitors from config, finaly apply it.
	id := dpy.Monitors.getMonitorsId()
	if len(id) == 0 {
		logger.Warning("No connected monitor found")
		return fmt.Errorf("No output connected")
	}

	if dpy.DisplayMode == DisplayModeCustom {
		id = dpy.CurrentCustomId
	}

	cMonitor := dpy.config.get(id)
	if cMonitor == nil {
		logger.Warning("No config found for:", id)
		return fmt.Errorf("No config found for '%s'", id)
	}
	err := dpy.applyConfigSettings(cMonitor)
	if err != nil {
		logger.Warning("[ResetChanges] apply config failed:", err)
		return err
	}
	dpy.setPropHasChanged(false)
	return nil
}

func (dpy *Manager) Save() error {
	if len(dpy.outputInfos) != 1 && dpy.DisplayMode != DisplayModeCustom {
		// if multi-output and not custom mode, nothing
		return nil
	}

	id := dpy.Monitors.getMonitorsId()
	if len(id) == 0 {
		logger.Warning("No output connected")
		return fmt.Errorf("No output connected")
	}

	if dpy.DisplayMode == DisplayModeCustom {
		id = dpy.CurrentCustomId
	}
	cMonitor := configMonitor{
		Primary:   dpy.Primary,
		BaseInfos: dpy.Monitors.getBaseInfos(),
	}
	dpy.config.set(id, &cMonitor)

	err := dpy.config.writeFile()
	if err != nil {
		logger.Error("Save config failed:", err)
		return err
	}

	dpy.setPropHasChanged(false)
	if dpy.Primary != dpy.setting.GetString(gsKeyPrimary) {
		dpy.setting.SetString(gsKeyPrimary, dpy.Primary)
	}
	return nil
}

func (dpy *Manager) DeleteCustomMode(id string) error {
	if id == "" {
		logger.Warning("Empty mode id")
		return fmt.Errorf("The mode id is empty")
	}

	if !dpy.isIdDeletable(id) {
		logger.Warningf("The mode '%s' was used currently", id)
		return fmt.Errorf("'%s' was used currently", id)
	}

	if !dpy.config.delete(id) {
		// no config id found
		return nil
	}

	dpy.setPropCustomIdList(dpy.getCustomIdList())
	return dpy.config.writeFile()
}

func (dpy *Manager) Reset() error {
	// remove config file
	os.Remove(dpy.config.filename)
	dpy.config = &configManager{
		BaseGroup: make(map[string]*configMonitor),
		filename:  configFile,
	}
	dpy.syncCurrentCustomId("")
	dpy.setPropCustomIdList(dpy.getCustomIdList())
	err := dpy.SwitchMode(DisplayModeExtend, "")
	if err != nil {
		logger.Error("[Reset] switch to extend failed:", err)
		return err
	}
	dpy.setting.Reset(gsKeyBrightness)
	dpy.initBrightness()
	dpy.setting.Reset(gsKeyMapOutput)
	dpy.initTouchMap()
	return nil
}

func (dpy *Manager) AssociateTouch(output, touch string) error {
	if dpy.TouchMap[touch] == output {
		return nil
	}

	err := dpy.doSetTouchMap(output, touch)
	if err != nil {
		logger.Warning("[AssociateTouch] set failed:", err)
		return err
	}

	dpy.TouchMap[touch] = output
	dpy.setPropTouchMap(dpy.TouchMap)
	dpy.setting.SetString(gsKeyMapOutput, jsonMarshal(dpy.TouchMap))
	return nil
}

func (dpy *Manager) RefreshBrightness() {
	for k, v := range dpy.Brightness {
		dpy.doSetBrightness(v, k)
	}
}
