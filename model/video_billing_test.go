package model

import (
	"maps"
	"os"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestVideoBillingDatabaseMatrix(t *testing.T) {
	common.OptionMapRWMutex.Lock()
	previousOptions := maps.Clone(common.OptionMap)
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})
	source, err := os.ReadFile("../plugins/tasks/sora/plugin.js")
	require.NoError(t, err)
	plugin, err := jsplugin.DefaultRegistry.Register(string(source), jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister("sora") })
	_, err = jsplugin.DefaultRegistry.Register(`
export const meta={apiVersion:1,key:"native-video-billing",name:"Native video",version:"1.0.0",author:{name:"Test"},models:["native-video-billing"],fetchMode:"per_task",
usageSchema:{seconds:{type:"number",unit:"second"},resolution:{enum:["768P","2K"]}}};
export function buildSubmitRequest(){return {};}
export function parseSubmitResponse(){return {};}
export function buildQueryRequest(){return {};}
export function parseTaskResult(){return {};}
`, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister("native-video-billing") })
	savedSettings := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		savedSettings[key] = value
		return nil
	}))
	t.Cleanup(func() { require.NoError(t, config.GlobalConfig.LoadFromDB(savedSettings)) })
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			require.NoError(t, config.GlobalConfig.LoadFromDB(savedSettings))
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(":memory:")
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN is not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN is not configured")
				}
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			previousDB, previousType, previousRaw := DB, common.MainDatabaseType(), videoBillingRaw()
			DB = db
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			initCol()
			t.Cleanup(func() {
				DB = previousDB
				common.SetMainDatabaseType(previousType)
				initCol()
				require.NoError(t, updateOptionMap(billing_setting.VideoBillingOption, previousRaw))
				assert.NoError(t, sqlDB.Close())
			})
			require.NoError(t, db.AutoMigrate(&Option{}, &Channel{}, &Ability{}, &Model{}, &Vendor{}))
			require.NoError(t, updateOptionMap(billing_setting.VideoBillingOption, ""))
			var version string
			query := "SELECT version()"
			if dialect == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("database version: %s", version)

			channel := &Channel{Id: 84301, Type: constant.ChannelTypeOpenAI, Name: "video billing test", Models: "minimax_h3", Status: 1}
			require.NoError(t, db.Create(channel).Error)
			const expression = `u("resolution") == "480p" ? tier("480p", u("seconds") * 0.05) : u("resolution") == "720p" ? tier("720p", u("seconds") * 0.075) : tier("1080p", u("seconds") * 0.1)`
			draft := PricingValues{billing_setting.PluginBillingExprOption: map[string]any{"sora": expression}}
			require.NoError(t, ValidateModelPricing("minimax_h3", draft), "a model price must not require a separate channel association")
			assert.False(t, HasVideoBillingModel("minimax_h3"))
			before, err := GetVideoBillingConfig()
			require.NoError(t, err)
			require.NoError(t, UpdateVideoBillingBindings(VideoBillingBindings{ChannelIDs: []int{channel.Id}, Models: []string{}}, before.Version))
			require.ErrorIs(t, UpdateVideoBillingBindings(VideoBillingBindings{}, before.Version), ErrModelPricingConflict)
			require.NoError(t, ValidateModelPricing("minimax_h3", draft))
			assert.False(t, HasVideoBillingModel("chat-model"))
			_, declared := jsplugin.DefaultRegistry.Generation().CanonicalModel("minimax_h3")
			assert.False(t, declared, "billing association must not register a route")

			snapshot, err := GetModelPricingSnapshot([]string{"minimax_h3"})
			require.NoError(t, err)
			require.Len(t, snapshot.Entries, 1)
			require.NoError(t, UpdateModelPricing([]ModelPricingChange{{
				ModelName: "minimax_h3", ExpectedVersion: snapshot.Entries[0].Version, Pricing: draft,
			}}))
			selectedExpr, overridden, exists := billing_setting.ResolveTaskBillingExpr("sora", "minimax_h3", "vendor-alias")
			assert.True(t, exists)
			assert.True(t, overridden)
			assert.Equal(t, expression, selectedExpr)
			assert.True(t, HasVideoBillingModel("minimax_h3"))
			snapshot, err = GetModelPricingSnapshot([]string{"minimax_h3"})
			require.NoError(t, err)
			require.Len(t, snapshot.Entries[0].PluginVariants, 1)
			assert.Equal(t, billing_setting.VideoUsageSchema(), snapshot.Entries[0].PluginVariants[0].UsageSchema)
			assert.False(t, snapshot.Entries[0].PluginVariants[0].Stale)

			otherChannel := &Channel{Id: 84302, Type: constant.ChannelTypeOpenAI, Name: "other video channel", Models: "other-video", Status: 1}
			require.NoError(t, db.Create(otherChannel).Error)
			bindings, err := GetVideoBillingConfig()
			require.NoError(t, err)
			require.NoError(t, UpdateVideoBillingBindings(VideoBillingBindings{ChannelIDs: []int{otherChannel.Id}}, bindings.Version))
			assert.True(t, HasVideoBillingModel("minimax_h3"), "switching discovery channels must not disable a saved model price")
			snapshot, err = GetModelPricingSnapshot([]string{"minimax_h3"})
			require.NoError(t, err)
			assert.False(t, snapshot.Entries[0].PluginVariants[0].Stale)
			assert.Equal(t, expression, snapshot.Entries[0].PluginVariants[0].Configured)

			require.NoError(t, db.Model(channel).Update("models", "minimax_h3,future-video").Error)
			assert.False(t, HasVideoBillingModel("future-video"), "discovery must not enable billing without a model price")
			require.NoError(t, ValidateModelPricing("future-video", draft))
			newSnapshot, err := GetModelPricingSnapshot([]string{"future-video", "minimax_h3"})
			require.NoError(t, err)
			require.ErrorIs(t, UpdateModelPricing([]ModelPricingChange{
				{ModelName: "future-video", ExpectedVersion: newSnapshot.Entries[0].Version, Pricing: draft},
				{ModelName: "minimax_h3", ExpectedVersion: "stale", Pricing: draft},
			}), ErrModelPricingConflict)
			afterConflict, err := GetModelPricingSnapshot([]string{"future-video"})
			require.NoError(t, err)
			assert.Empty(t, afterConflict.Entries[0].Configured, "conflicts must roll back the entire batch")
			require.NoError(t, db.Model(channel).Update("models", "minimax_h3,future-video,native-video-billing").Error)
			assert.True(t, VideoBillingRequiresProviderPrice(jsplugin.DefaultRegistry.Generation(), "native-video-billing"))
			assert.False(t, VideoBillingRequiresProviderPrice(jsplugin.DefaultRegistry.Generation(), "future-video"))
			assert.False(t, jsplugin.DefaultRegistry.Generation().SharedModel("native-video-billing"), "billing must detect the added provider without changing routing")
			nativeSnapshot, err := GetModelPricingSnapshot([]string{"native-video-billing"})
			require.NoError(t, err)
			nativeExpression := `u("resolution") == "768P" ? tier("768P", u("seconds") * 0.1) : tier("2K", u("seconds") * 0.2)`
			require.NoError(t, UpdateModelPricing([]ModelPricingChange{{
				ModelName: "native-video-billing", ExpectedVersion: nativeSnapshot.Entries[0].Version,
				Pricing: PricingValues{"billing_setting.billing_mode": "tiered_expr", "billing_setting.billing_expr": nativeExpression},
			}}))
			nativeSnapshot, err = GetModelPricingSnapshot([]string{"native-video-billing"})
			require.NoError(t, err)
			for _, variant := range nativeSnapshot.Entries[0].PluginVariants {
				assert.NotEqual(t, "sora", variant.PluginKey, "channel selection must not add an unconfigured video provider")
			}

			current, err := GetModelPricingSnapshot([]string{"minimax_h3"})
			require.NoError(t, err)
			require.NoError(t, UpdateModelPricing([]ModelPricingChange{{
				ModelName: "minimax_h3", ExpectedVersion: current.Entries[0].Version, Pricing: PricingValues{},
			}}))
			assert.False(t, HasVideoBillingModel("minimax_h3"), "removing a provider price must disable generic video billing immediately")
			require.ErrorIs(t, UpdateModelPricing([]ModelPricingChange{{
				ModelName: "minimax_h3", ExpectedVersion: current.Entries[0].Version, Pricing: draft,
			}}), ErrModelPricingConflict)

			for _, tc := range []struct {
				resolution string
				cost       float64
			}{{"480p", 0.3}, {"720p", 0.45}, {"1080p", 0.6}} {
				facts := map[string]any{"seconds": float64(6), "resolution": tc.resolution}
				require.NoError(t, billing_setting.ValidateVideoExprFacts(expression, facts))
				cost, _, err := billingexpr.RunExprWithRequest(expression, billingexpr.TokenParams{}, billingexpr.RequestInput{Usage: facts})
				require.NoError(t, err)
				assert.InDelta(t, tc.cost, cost, 1e-12)
			}
			for _, resolution := range []any{nil, "auto", "4k"} {
				assert.Error(t, billing_setting.ValidateVideoExprFacts(expression, map[string]any{"resolution": resolution}))
			}
			require.NoError(t, billing_setting.ValidateVideoExprFacts(`tier("base", 0.3)`, nil))
			require.Error(t, ValidateModelPricing("minimax_h3", PricingValues{billing_setting.PluginBillingExprOption: map[string]any{"sora": `tier("base", fixed(0.3))`}}))
			value, err := plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{
				"model": "minimax_h3", "requestBody": map[string]any{"seconds": "6", "metadata": map[string]any{"resolution": "480p"}},
			})
			require.NoError(t, err)
			encoded, err := common.Marshal(value)
			require.NoError(t, err)
			var facts map[string]any
			require.NoError(t, common.Unmarshal(encoded, &facts))
			assert.Equal(t, float64(6), facts["seconds"])
			assert.Equal(t, "480p", facts["resolution"])
		})
	}
}
