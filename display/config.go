package display

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"github.com/linuxdeepin/go-x11-client/ext/randr"
	"pkg.deepin.io/lib/log"
	"pkg.deepin.io/lib/xdg/basedir"
)

// 目前最新配置文件版本
const configVersion = "5.0"

var (
	// 旧版本配置文件，~/.config/deepin/startdde/display.json
	configFile string
	// 目前最新版本配置文件， ~/.config/deepin/startdde/display_v5.json
	configFile_v5 string
	// ~/.config/deepin/startdde/config.version
	configVersionFile string
	// 内置显示器配置文件，~/.config/deepin/startdde/builtin-monitor
	builtinMonitorConfigFile string
)

func init() {
	cfgDir := filepath.Join(basedir.GetUserConfigDir(), "deepin/startdde")
	configFile = filepath.Join(cfgDir, "display.json")
	configFile_v5 = filepath.Join(cfgDir, "display_v5.json")
	configVersionFile = filepath.Join(cfgDir, "config.version")
	builtinMonitorConfigFile = filepath.Join(cfgDir, "builtin-monitor")
}

type Config map[string]*ScreenConfig

type ConfigV6 struct {
	ConfigV5 Config
	FillMode *FillModeConfigs
}

type FillModeConfigs struct {
	FillModeMap map[string]string
}

type ScreenConfig struct {
	Mirror  *ModeConfigs      `json:",omitempty"`
	Extend  *ModeConfigs      `json:",omitempty"`
	OnlyOne *ModeConfigs      `json:",omitempty"`
	Single  *SingleModeConfig `json:",omitempty"`
}

type ModeConfigs struct {
	Monitors []*MonitorConfig
}

type SingleModeConfig struct {
	// 这里其实不能用 Monitors，因为是单数
	Monitor                *MonitorConfig `json:"Monitors"` // 单屏时,该配置文件中色温相关数据未生效;增加json的tag是为了兼容之前配置文件
	ColorTemperatureMode   int32
	ColorTemperatureManual int32
}

func (s *ScreenConfig) getMonitorConfigs(mode uint8) []*MonitorConfig {
	switch mode {
	case DisplayModeMirror:
		if s.Mirror == nil {
			return nil
		}
		return s.Mirror.Monitors

	case DisplayModeExtend:
		if s.Extend == nil {
			return nil
		}
		return s.Extend.Monitors

	case DisplayModeOnlyOne:
		if s.OnlyOne == nil {
			return nil
		}
		return s.OnlyOne.Monitors
	}

	return nil
}

func (s *ScreenConfig) getModeConfigs(mode uint8) *ModeConfigs {
	switch mode {
	case DisplayModeMirror:
		if s.Mirror == nil {
			s.Mirror = &ModeConfigs{}
		}
		return s.Mirror

	case DisplayModeExtend:
		if s.Extend == nil {
			s.Extend = &ModeConfigs{}
		}
		return s.Extend

	case DisplayModeOnlyOne:
		if s.OnlyOne == nil {
			s.OnlyOne = &ModeConfigs{}
		}
		return s.OnlyOne
	}

	return nil
}

func getMonitorConfigByUuid(configs []*MonitorConfig, uuid string) *MonitorConfig {
	for _, mc := range configs {
		if mc.UUID == uuid {
			return mc
		}
	}
	return nil
}

func getMonitorConfigPrimary(configs []*MonitorConfig) *MonitorConfig { //unused
	for _, mc := range configs {
		if mc.Primary {
			return mc
		}
	}
	return &MonitorConfig{}
}

func setMonitorConfigsPrimary(configs []*MonitorConfig, uuid string) {
	for _, mc := range configs {
		if mc.UUID == uuid {
			mc.Primary = true
		} else {
			mc.Primary = false
		}
	}
}

func updateMonitorConfigsName(configs []*MonitorConfig, monitorMap map[randr.Output]*Monitor) {
	for _, mc := range configs {
		for _, m := range monitorMap {
			if mc.UUID == m.uuid {
				mc.Name = m.Name
				break
			}
		}
	}
}

func (s *ScreenConfig) setMonitorConfigs(mode uint8, configs []*MonitorConfig) {
	switch mode {
	case DisplayModeMirror:
		if s.Mirror == nil {
			s.Mirror = &ModeConfigs{}
		}
		s.Mirror.Monitors = configs

	case DisplayModeExtend:
		if s.Extend == nil {
			s.Extend = &ModeConfigs{}
		}
		s.Extend.Monitors = configs

	case DisplayModeOnlyOne:
		s.setMonitorConfigsOnlyOne(configs)
	}
}

func (s *ScreenConfig) setModeConfigs(mode uint8, colorTemperatureMode int32, colorTemperatureManual int32, monitorConfig []*MonitorConfig) {
	s.setMonitorConfigs(mode, monitorConfig)
	cfg := s.getModeConfigs(mode)
	for _, monitorConfig := range cfg.Monitors {
		if monitorConfig.Enabled {
			monitorConfig.ColorTemperatureMode = colorTemperatureMode
			monitorConfig.ColorTemperatureManual = colorTemperatureManual
		}
	}
}

func (s *ScreenConfig) setMonitorConfigsOnlyOne(configs []*MonitorConfig) {
	if s.OnlyOne == nil {
		s.OnlyOne = &ModeConfigs{}
	}
	oldConfigs := s.OnlyOne.Monitors
	var newConfigs []*MonitorConfig
	for _, cfg := range configs {
		if !cfg.Enabled {
			oldCfg := getMonitorConfigByUuid(oldConfigs, cfg.UUID)
			if oldCfg != nil {
				// 不设置 X,Y 是因为它们总是 0
				cfg.Width = oldCfg.Width
				cfg.Height = oldCfg.Height
				cfg.RefreshRate = oldCfg.RefreshRate
				cfg.Rotation = oldCfg.Rotation
				cfg.Reflect = oldCfg.Reflect
			} else {
				continue
			}
		}
		newConfigs = append(newConfigs, cfg)
	}
	s.OnlyOne.Monitors = newConfigs
}

type MonitorConfig struct {
	UUID        string
	Name        string
	Enabled     bool
	X           int16
	Y           int16
	Width       uint16
	Height      uint16
	Rotation    uint16
	Reflect     uint16
	RefreshRate float64
	Brightness  float64
	Primary     bool

	ColorTemperatureMode   int32
	ColorTemperatureManual int32
}

func loadConfigV5(filename string) (Config, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var c Config
	err = json.Unmarshal(data, &c)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func loadConfigV6(filename string) (ConfigV6, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return ConfigV6{}, err
	}

	var c ConfigV6
	err = json.Unmarshal(data, &c)
	if err != nil {
		return ConfigV6{}, err
	}

	if c.FillMode == nil {
		c.FillMode = &FillModeConfigs{}
	}

	// 存在，没有V6的情况，只有V5,将此时数据读取存到V6
	if c.ConfigV5 == nil {
		var configV5 Config
		err = json.Unmarshal(data, &configV5)
		if err != nil {
			return ConfigV6{}, err
		}

		c.ConfigV5 = configV5
	}
	return c, nil
}

func loadConfig(m *Manager) (config Config) {
	cfgVer, err := getConfigVersion(configVersionFile)
	if err == nil {
		//3.3配置文件转换
		if cfgVer == "3.3" {
			cfg0, err := loadConfigV3_3(configFile)
			if err == nil {
				config = cfg0.toConfig(m)
			} else if !os.IsNotExist(err) {
				logger.Warning(err)
			}
		} else if cfgVer == "4.0" { //4.0配置文件转换
			cfg0, err := loadConfigV4(configFile)
			if err == nil {
				config = cfg0.toConfig(m)
			} else if !os.IsNotExist(err) {
				logger.Warning(err)
			}
		}
	} else if !os.IsNotExist(err) {
		logger.Warning(err)
	}

	if len(config) == 0 {
		configV6, err := loadConfigV6(configFile_v5)
		if err != nil {
			// 加载 v5 和 v6 配置文件都失败
			config = make(Config)
			//配置文件为空，且当前模式为自定义，则设置当前模式为复制模式
			if m.DisplayMode == DisplayModeCustom {
				m.DisplayMode = DisplayModeMirror
			}
			if !os.IsNotExist(err) {
				logger.Warning(err)
			}
			m.configV6.ConfigV5 = config
			m.configV6.FillMode = &FillModeConfigs{}
		} else {
			// 加载 v5 或 v6 配置文件成功
			config = configV6.ConfigV5
			m.configV6.FillMode = configV6.FillMode
		}
		if m.configV6.FillMode.FillModeMap == nil {
			m.configV6.FillMode.FillModeMap = make(map[string]string)
		}
	} else {
		// 加载 v5 之前配置文件成功
		m.configV6.FillMode = &FillModeConfigs{
			FillModeMap: make(map[string]string),
		}
	}

	if logger.GetLogLevel() == log.LevelDebug {
		logger.Debug("load config:", spew.Sdump(config))
	}
	logger.Debugf("loadConfig fillMode: %#v", m.configV6.FillMode)
	return
}

func (c ConfigV6) save(filename string) error {
	var data []byte
	var err error
	if logger.GetLogLevel() == log.LevelDebug {
		data, err = json.MarshalIndent(c, "", "    ")
		if err != nil {
			return err
		}
	} else {
		data, err = json.Marshal(c)
		if err != nil {
			return err
		}
	}

	err = ioutil.WriteFile(filename, data, 0644)
	if err != nil {
		return err
	}
	return nil
}

func loadBuiltinMonitorConfig(filename string) (string, error) {
	content, err := ioutil.ReadFile(filename)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(content)), nil
}

func saveBuiltinMonitorConfig(filename, name string) error {
	dir := filepath.Dir(filename)
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(filename, []byte(name), 0644)
	if err != nil {
		return err
	}
	return nil
}
