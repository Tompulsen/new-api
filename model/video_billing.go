package model

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type VideoBillingBindings struct {
	ChannelIDs []int    `json:"channel_ids"`
	Models     []string `json:"models"`
}

type VideoBillingChannel struct {
	ID     int      `json:"id"`
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

type VideoBillingConfig struct {
	Bindings VideoBillingBindings                 `json:"bindings"`
	Version  string                               `json:"version"`
	Channels []VideoBillingChannel                `json:"channels"`
	Models   []string                             `json:"models"`
	Schema   map[string]jsplugin.UsageFieldSchema `json:"usage_schema"`
}

type videoBillingView struct {
	raw       string
	expiresAt time.Time
	models    map[string]bool
}

var videoBillingCache atomic.Pointer[videoBillingView]
var videoBillingMu sync.Mutex

func videoBillingRaw() string {
	common.OptionMapRWMutex.RLock()
	defer common.OptionMapRWMutex.RUnlock()
	return common.OptionMap[billing_setting.VideoBillingOption]
}

func videoBillingBindings(raw string) VideoBillingBindings {
	result := VideoBillingBindings{ChannelIDs: []int{}, Models: []string{}}
	if raw != "" {
		if err := common.UnmarshalJsonStr(raw, &result); err != nil {
			common.SysError(fmt.Sprintf("invalid video billing bindings: %s", err))
		}
	}
	if result.ChannelIDs == nil {
		result.ChannelIDs = []int{}
	}
	if result.Models == nil {
		result.Models = []string{}
	}
	return result
}

func videoBillingVersion(raw string) string {
	return ModelPricingVersion(PricingValues{"bindings": videoBillingBindings(raw)})
}

func compatibleVideoBillingChannel(channel *Channel) bool {
	if channel.Type == constant.ChannelTypeOpenAI || channel.Type == constant.ChannelTypeSora {
		return true
	}
	if channel.Type == constant.ChannelTypeTaskPlugin || channel.Type == constant.ChannelTypeNewAPI {
		return slices.Contains(channel.GetSetting().TaskPluginBindings(), "sora")
	}
	return false
}

func GetVideoBillingConfig() (*VideoBillingConfig, error) {
	raw := videoBillingRaw()
	bindings := videoBillingBindings(raw)
	var channels []Channel
	if err := DB.Select("id", "name", "type", "models", "setting").Find(&channels).Error; err != nil {
		return nil, err
	}
	result := &VideoBillingConfig{
		Bindings: bindings, Version: videoBillingVersion(raw),
		Channels: []VideoBillingChannel{}, Models: []string{}, Schema: billing_setting.VideoUsageSchema(),
	}
	models := make(map[string]bool)
	for _, name := range bindings.Models {
		models[name] = true
	}
	for i := range channels {
		channel := &channels[i]
		if !compatibleVideoBillingChannel(channel) {
			continue
		}
		result.Channels = append(result.Channels, VideoBillingChannel{ID: channel.Id, Name: channel.Name, Models: channel.GetModels()})
		if slices.Contains(bindings.ChannelIDs, channel.Id) {
			for _, name := range channel.GetModels() {
				models[name] = true
			}
		}
	}
	for name := range models {
		result.Models = append(result.Models, name)
	}
	slices.Sort(result.Models)
	slices.SortFunc(result.Channels, func(a, b VideoBillingChannel) int { return a.ID - b.ID })
	return result, nil
}

func HasVideoBillingModel(name string) bool {
	return videoBillingModels()[name]
}

func videoBillingModels() map[string]bool {
	if DB == nil {
		return nil
	}
	raw := videoBillingRaw()
	if raw == "" {
		return nil
	}
	view := videoBillingCache.Load()
	if view != nil && view.raw == raw && time.Now().Before(view.expiresAt) {
		return view.models
	}
	videoBillingMu.Lock()
	defer videoBillingMu.Unlock()
	view = videoBillingCache.Load()
	if view != nil && view.raw == raw && time.Now().Before(view.expiresAt) {
		return view.models
	}
	cfg, err := GetVideoBillingConfig()
	if err != nil {
		common.SysError(fmt.Sprintf("load video billing bindings: %s", err))
		return nil
	}
	view = &videoBillingView{raw: raw, expiresAt: time.Now().Add(time.Minute), models: make(map[string]bool)}
	for _, model := range cfg.Models {
		view.models[model] = true
	}
	videoBillingCache.Store(view)
	return view.models
}

// This metadata index never participates in route or channel selection.
func TaskPricingPlugins(generation *jsplugin.RoutingGeneration, name string) []*jsplugin.LoadedPlugin {
	return taskPricingPlugins(generation, name, videoBillingModels())
}

func taskPricingPlugins(generation *jsplugin.RoutingGeneration, name string, videoModels map[string]bool) []*jsplugin.LoadedPlugin {
	plugins := slices.Clone(generation.PluginsByModel(name))
	if videoModels[name] {
		if sora, ok := generation.Get("sora"); ok && !slices.Contains(plugins, sora) {
			plugins = append(plugins, sora)
		}
	}
	return plugins
}

func TaskPricingUsage(plugin *jsplugin.LoadedPlugin, names ...string) (map[string]jsplugin.UsageFieldSchema, []jsplugin.UsageExample) {
	if plugin.Meta.Key != "sora" {
		return plugin.Meta.UsageForModels(names...)
	}
	return taskPricingUsage(plugin, videoBillingModels(), names...)
}

func taskPricingUsage(plugin *jsplugin.LoadedPlugin, videoModels map[string]bool, names ...string) (map[string]jsplugin.UsageFieldSchema, []jsplugin.UsageExample) {
	if plugin.Meta.Key == "sora" {
		for _, name := range names {
			if videoModels[name] {
				return billing_setting.VideoUsageSchema(), nil
			}
		}
	}
	return plugin.Meta.UsageForModels(names...)
}

// Native providers can use the same fact names with different units or enum values.
func VideoBillingRequiresProviderPrice(generation *jsplugin.RoutingGeneration, names ...string) bool {
	if generation == nil {
		generation = jsplugin.DefaultRegistry.Generation()
	}
	bound := false
	for _, name := range names {
		bound = bound || HasVideoBillingModel(name)
	}
	if !bound {
		return false
	}
	for _, name := range names {
		for _, plugin := range generation.PluginsByModel(name) {
			if plugin.Meta.Key != "sora" {
				return true
			}
		}
		if target, ok := ResolveTaskModelAlias(generation, name); ok && target.PluginKey != "sora" {
			return true
		}
	}
	return false
}

func UpdateVideoBillingBindings(bindings VideoBillingBindings, expectedVersion string) error {
	if bindings.ChannelIDs == nil {
		bindings.ChannelIDs = []int{}
	}
	if bindings.Models == nil {
		bindings.Models = []string{}
	}
	if len(bindings.ChannelIDs) > 1000 || len(bindings.Models) > 10000 {
		return errors.New("too many video billing bindings")
	}
	cfg, err := GetVideoBillingConfig()
	if err != nil {
		return err
	}
	for _, id := range bindings.ChannelIDs {
		if id <= 0 || !slices.ContainsFunc(cfg.Channels, func(channel VideoBillingChannel) bool { return channel.ID == id }) {
			return fmt.Errorf("channel %d is not an OpenAI Video compatible channel", id)
		}
	}
	for _, name := range bindings.Models {
		if name == "" || name != strings.TrimSpace(name) || len(name) > 256 || strings.ContainsAny(name, ",\r\n") {
			return errors.New("invalid video billing model name")
		}
	}
	if _, ok := jsplugin.DefaultRegistry.Generation().Get("sora"); !ok {
		return errors.New("enable the OpenAI Video compatible plugin before configuring video billing")
	}
	slices.Sort(bindings.ChannelIDs)
	bindings.ChannelIDs = slices.Compact(bindings.ChannelIDs)
	slices.Sort(bindings.Models)
	bindings.Models = slices.Compact(bindings.Models)
	encoded, err := common.Marshal(bindings)
	if err != nil {
		return err
	}
	err = DB.Transaction(func(tx *gorm.DB) error {
		row := Option{Key: billing_setting.VideoBillingOption, Value: ""}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where(commonKeyCol+" = ?", row.Key).First(&row).Error; err != nil {
			return err
		}
		if expectedVersion == "" || videoBillingVersion(row.Value) != expectedVersion {
			return ErrModelPricingConflict
		}
		return tx.Model(&row).Update("value", string(encoded)).Error
	})
	if err != nil {
		return err
	}
	if err := updateOptionMap(billing_setting.VideoBillingOption, string(encoded)); err != nil {
		return err
	}
	videoBillingCache.Store(nil)
	InvalidatePricingCache()
	return nil
}
