package chunk

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/common/model"
	"github.com/weaveworks/common/mtime"
	yaml "gopkg.in/yaml.v2"

	"github.com/cortexproject/cortex/pkg/util"
)

const (
	secondsInHour      = int64(time.Hour / time.Second)
	secondsInDay       = int64(24 * time.Hour / time.Second)
	millisecondsInHour = int64(time.Hour / time.Millisecond)
	millisecondsInDay  = int64(24 * time.Hour / time.Millisecond)
)

var (
	errInvalidSchemaVersion = errors.New("invalid schema version")
	errInvalidTablePeriod   = errors.New("the table period must be a multiple of 24h (1h for schema v1)")
	errConfigFileNotSet     = errors.New("schema config file needs to be set")
)

// PeriodConfig defines the schema and tables to use for a period of time
type PeriodConfig struct {
	From        DayTime             `yaml:"from"`         // used when working with config
	IndexType   string              `yaml:"store"`        // type of index client to use.
	ObjectType  string              `yaml:"object_store"` // type of object client to use; if omitted, defaults to store.
	Schema      string              `yaml:"schema"`
	IndexTables PeriodicTableConfig `yaml:"index"`
	ChunkTables PeriodicTableConfig `yaml:"chunks,omitempty"`
	RowShards   uint32              `yaml:"row_shards"`
}

// DayTime is a model.Time what holds day-aligned values, and marshals to/from
// YAML in YYYY-MM-DD format.
type DayTime struct {
	model.Time
}

// MarshalYAML implements yaml.Marshaller.
func (d DayTime) MarshalYAML() (interface{}, error) {
	return d.Time.Time().Format("2006-01-02"), nil
}

// UnmarshalYAML implements yaml.Unmarshaller.
func (d *DayTime) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var from string
	if err := unmarshal(&from); err != nil {
		return err
	}
	t, err := time.Parse("2006-01-02", from)
	if err != nil {
		return err
	}
	d.Time = model.TimeFromUnix(t.Unix())
	return nil
}

// SchemaConfig contains the config for our chunk index schemas
type SchemaConfig struct {
	Configs []PeriodConfig `yaml:"configs"`

	fileName       string
	legacyFileName string
}

// RegisterFlags adds the flags required to config this to the given FlagSet.
func (cfg *SchemaConfig) RegisterFlags(f *flag.FlagSet) {
	flag.StringVar(&cfg.fileName, "schema-config-file", "", "The path to the schema config file.")
	// TODO(gouthamve): Add a metric for this.
	flag.StringVar(&cfg.legacyFileName, "config-yaml", "", "DEPRECATED(use -schema-config-file) The path to the schema config file.")
}

// loadFromFile loads the schema config from a yaml file
func (cfg *SchemaConfig) loadFromFile() error {
	if cfg.fileName == "" {
		cfg.fileName = cfg.legacyFileName

		if cfg.legacyFileName != "" {
			level.Warn(util.Logger).Log("msg", "running with DEPRECATED flag -config-yaml, use -schema-config-file instead")
		}
	}

	if cfg.fileName == "" {
		return errConfigFileNotSet
	}

	f, err := os.Open(cfg.fileName)
	if err != nil {
		return err
	}

	decoder := yaml.NewDecoder(f)
	decoder.SetStrict(true)
	return decoder.Decode(&cfg)
}

// Validate the schema config and returns an error if the validation
// doesn't pass
func (cfg *SchemaConfig) Validate() error {
	for _, periodCfg := range cfg.Configs {
		if err := periodCfg.validate(); err != nil {
			return err
		}
	}
	return nil
}

func defaultRowShards(schema string) uint32 {
	switch schema {
	case "v1", "v2", "v3", "v4", "v5", "v6", "v9":
		return 0
	default:
		return 16
	}
}

// ForEachAfter will call f() on every entry after t, splitting
// entries if necessary so there is an entry starting at t
func (cfg *SchemaConfig) ForEachAfter(t model.Time, f func(config *PeriodConfig)) {
	for i := 0; i < len(cfg.Configs); i++ {
		if t > cfg.Configs[i].From.Time &&
			(i+1 == len(cfg.Configs) || t < cfg.Configs[i+1].From.Time) {
			// Split the i'th entry by duplicating then overwriting the From time
			cfg.Configs = append(cfg.Configs[:i+1], cfg.Configs[i:]...)
			cfg.Configs[i+1].From = DayTime{t}
		}
		if cfg.Configs[i].From.Time >= t {
			f(&cfg.Configs[i])
		}
	}
}

// CreateSchema returns the schema defined by the PeriodConfig
func (cfg PeriodConfig) CreateSchema() Schema {
	rowShards := defaultRowShards(cfg.Schema)
	if cfg.RowShards > 0 {
		rowShards = cfg.RowShards
	}

	var e entries
	switch cfg.Schema {
	case "v1":
		e = originalEntries{}
	case "v2":
		e = originalEntries{}
	case "v3":
		e = base64Entries{originalEntries{}}
	case "v4":
		e = labelNameInHashKeyEntries{}
	case "v5":
		e = v5Entries{}
	case "v6":
		e = v6Entries{}
	case "v9":
		e = v9Entries{}
	case "v10":
		e = v10Entries{
			rowShards: rowShards,
		}
	case "v11":
		e = v11Entries{
			v10Entries: v10Entries{
				rowShards: rowShards,
			},
		}
	default:
		return nil
	}

	buckets, _ := cfg.createBucketsFunc()

	return schema{buckets, e}
}

func (cfg PeriodConfig) createBucketsFunc() (schemaBucketsFunc, time.Duration) {
	switch cfg.Schema {
	case "v1":
		return cfg.hourlyBuckets, 1 * time.Hour
	default:
		return cfg.dailyBuckets, 24 * time.Hour
	}
}

// validate the period config
func (cfg PeriodConfig) validate() error {
	// Ensure the schema version exists
	schema := cfg.CreateSchema()
	if schema == nil {
		return errInvalidSchemaVersion
	}

	// Ensure the tables period is a multiple of the bucket period
	_, bucketsPeriod := cfg.createBucketsFunc()

	if cfg.IndexTables.Period > 0 && cfg.IndexTables.Period%bucketsPeriod != 0 {
		return errInvalidTablePeriod
	}

	if cfg.ChunkTables.Period > 0 && cfg.ChunkTables.Period%bucketsPeriod != 0 {
		return errInvalidTablePeriod
	}

	return nil
}

// Load the yaml file, or build the config from legacy command-line flags
func (cfg *SchemaConfig) Load() error {
	if len(cfg.Configs) > 0 {
		return nil
	}

	// Load config from file.
	if err := cfg.loadFromFile(); err != nil {
		return err
	}

	return cfg.Validate()
}

// Bucket describes a range of time with a tableName and hashKey
type Bucket struct {
	from       uint32
	through    uint32
	tableName  string
	hashKey    string
	bucketSize uint32 // helps with deletion of series ids in series store. Size in milliseconds.
}

func (cfg *PeriodConfig) hourlyBuckets(from, through model.Time, userID string) []Bucket {
	var (
		fromHour    = from.Unix() / secondsInHour
		throughHour = through.Unix() / secondsInHour
		result      = []Bucket{}
	)

	for i := fromHour; i <= throughHour; i++ {
		relativeFrom := util.Max64(0, int64(from)-(i*millisecondsInHour))
		relativeThrough := util.Min64(millisecondsInHour, int64(through)-(i*millisecondsInHour))
		result = append(result, Bucket{
			from:       uint32(relativeFrom),
			through:    uint32(relativeThrough),
			tableName:  cfg.IndexTables.TableFor(model.TimeFromUnix(i * secondsInHour)),
			hashKey:    fmt.Sprintf("%s:%d", userID, i),
			bucketSize: uint32(millisecondsInHour), // helps with deletion of series ids in series store
		})
	}
	return result
}

func (cfg *PeriodConfig) dailyBuckets(from, through model.Time, userID string) []Bucket {
	var (
		fromDay    = from.Unix() / secondsInDay
		throughDay = through.Unix() / secondsInDay
		result     = []Bucket{}
	)

	for i := fromDay; i <= throughDay; i++ {
		// The idea here is that the hash key contains the bucket start time (rounded to
		// the nearest day).  The range key can contain the offset from that, to the
		// (start/end) of the chunk. For chunks that span multiple buckets, these
		// offsets will be capped to the bucket boundaries, i.e. start will be
		// positive in the first bucket, then zero in the next etc.
		//
		// The reason for doing all this is to reduce the size of the time stamps we
		// include in the range keys - we use a uint32 - as we then have to base 32
		// encode it.

		relativeFrom := util.Max64(0, int64(from)-(i*millisecondsInDay))
		relativeThrough := util.Min64(millisecondsInDay, int64(through)-(i*millisecondsInDay))
		result = append(result, Bucket{
			from:       uint32(relativeFrom),
			through:    uint32(relativeThrough),
			tableName:  cfg.IndexTables.TableFor(model.TimeFromUnix(i * secondsInDay)),
			hashKey:    fmt.Sprintf("%s:d%d", userID, i),
			bucketSize: uint32(millisecondsInDay), // helps with deletion of series ids in series store
		})
	}
	return result
}

// PeriodicTableConfig is configuration for a set of time-sharded tables.
type PeriodicTableConfig struct {
	Prefix string        `yaml:"prefix"`
	Period time.Duration `yaml:"period,omitempty"`
	Tags   Tags          `yaml:"tags,omitempty"`
}

// AutoScalingConfig for DynamoDB tables.
type AutoScalingConfig struct {
	Enabled     bool    `yaml:"enabled,omitempty"`
	RoleARN     string  `yaml:"role_arn,omitempty"`
	MinCapacity int64   `yaml:"min_capacity,omitempty"`
	MaxCapacity int64   `yaml:"max_capacity,omitempty"`
	OutCooldown int64   `yaml:"out_cooldown,omitempty"`
	InCooldown  int64   `yaml:"in_cooldown,omitempty"`
	TargetValue float64 `yaml:"target,omitempty"`
}

// RegisterFlags adds the flags required to config this to the given FlagSet.
func (cfg *AutoScalingConfig) RegisterFlags(argPrefix string, f *flag.FlagSet) {
	f.BoolVar(&cfg.Enabled, argPrefix+".enabled", false, "Should we enable autoscale for the table.")
	f.StringVar(&cfg.RoleARN, argPrefix+".role-arn", "", "AWS AutoScaling role ARN")
	f.Int64Var(&cfg.MinCapacity, argPrefix+".min-capacity", 3000, "DynamoDB minimum provision capacity.")
	f.Int64Var(&cfg.MaxCapacity, argPrefix+".max-capacity", 6000, "DynamoDB maximum provision capacity.")
	f.Int64Var(&cfg.OutCooldown, argPrefix+".out-cooldown", 1800, "DynamoDB minimum seconds between each autoscale up.")
	f.Int64Var(&cfg.InCooldown, argPrefix+".in-cooldown", 1800, "DynamoDB minimum seconds between each autoscale down.")
	f.Float64Var(&cfg.TargetValue, argPrefix+".target-value", 80, "DynamoDB target ratio of consumed capacity to provisioned capacity.")
}

func (cfg *PeriodicTableConfig) periodicTables(from, through model.Time, pCfg ProvisionConfig, beginGrace, endGrace time.Duration, retention time.Duration) []TableDesc {
	var (
		periodSecs     = int64(cfg.Period / time.Second)
		beginGraceSecs = int64(beginGrace / time.Second)
		endGraceSecs   = int64(endGrace / time.Second)
		firstTable     = from.Unix() / periodSecs
		lastTable      = through.Unix() / periodSecs
		tablesToKeep   = int64(int64(retention/time.Second) / periodSecs)
		now            = mtime.Now().Unix()
		nowWeek        = now / periodSecs
		result         = []TableDesc{}
	)
	// If interval ends exactly on a period boundary, don’t include the upcoming period
	if through.Unix()%periodSecs == 0 {
		lastTable--
	}
	// Don't make tables further back than the configured retention
	if retention > 0 && lastTable > tablesToKeep && lastTable-firstTable >= tablesToKeep {
		firstTable = lastTable - tablesToKeep
	}
	for i := firstTable; i <= lastTable; i++ {
		table := TableDesc{
			Name:              cfg.tableForPeriod(i),
			ProvisionedRead:   pCfg.InactiveReadThroughput,
			ProvisionedWrite:  pCfg.InactiveWriteThroughput,
			UseOnDemandIOMode: pCfg.InactiveThroughputOnDemandMode,
			Tags:              cfg.Tags,
		}
		level.Debug(util.Logger).Log("msg", "Expected Table", "tableName", table.Name,
			"provisionedRead", table.ProvisionedRead,
			"provisionedWrite", table.ProvisionedWrite,
			"useOnDemandMode", table.UseOnDemandIOMode,
		)

		// if now is within table [start - grace, end + grace), then we need some write throughput
		if (i*periodSecs)-beginGraceSecs <= now && now < (i*periodSecs)+periodSecs+endGraceSecs {
			table.ProvisionedRead = pCfg.ProvisionedReadThroughput
			table.ProvisionedWrite = pCfg.ProvisionedWriteThroughput
			table.UseOnDemandIOMode = pCfg.ProvisionedThroughputOnDemandMode

			if pCfg.WriteScale.Enabled {
				table.WriteScale = pCfg.WriteScale
				table.UseOnDemandIOMode = false
			}

			if pCfg.ReadScale.Enabled {
				table.ReadScale = pCfg.ReadScale
				table.UseOnDemandIOMode = false
			}
			level.Debug(util.Logger).Log("msg", "Table is Active",
				"tableName", table.Name,
				"provisionedRead", table.ProvisionedRead,
				"provisionedWrite", table.ProvisionedWrite,
				"useOnDemandMode", table.UseOnDemandIOMode,
				"useWriteAutoScale", table.WriteScale.Enabled,
				"useReadAutoScale", table.ReadScale.Enabled)

		} else if pCfg.InactiveWriteScale.Enabled || pCfg.InactiveReadScale.Enabled {
			// Autoscale last N tables
			// this is measured against "now", since the lastWeek is the final week in the schema config range
			// the N last tables in that range will always be set to the inactive scaling settings.
			if pCfg.InactiveWriteScale.Enabled && i >= (nowWeek-pCfg.InactiveWriteScaleLastN) {
				table.WriteScale = pCfg.InactiveWriteScale
				table.UseOnDemandIOMode = false
			}
			if pCfg.InactiveReadScale.Enabled && i >= (nowWeek-pCfg.InactiveReadScaleLastN) {
				table.ReadScale = pCfg.InactiveReadScale
				table.UseOnDemandIOMode = false
			}

			level.Debug(util.Logger).Log("msg", "Table is Inactive",
				"tableName", table.Name,
				"provisionedRead", table.ProvisionedRead,
				"provisionedWrite", table.ProvisionedWrite,
				"useOnDemandMode", table.UseOnDemandIOMode,
				"useWriteAutoScale", table.WriteScale.Enabled,
				"useReadAutoScale", table.ReadScale.Enabled)
		}

		result = append(result, table)
	}
	return result
}

// ChunkTableFor calculates the chunk table shard for a given point in time.
func (cfg SchemaConfig) ChunkTableFor(t model.Time) (string, error) {
	for i := range cfg.Configs {
		if t >= cfg.Configs[i].From.Time && (i+1 == len(cfg.Configs) || t < cfg.Configs[i+1].From.Time) {
			return cfg.Configs[i].ChunkTables.TableFor(t), nil
		}
	}
	return "", fmt.Errorf("no chunk table found for time %v", t)
}

// TableFor calculates the table shard for a given point in time.
func (cfg *PeriodicTableConfig) TableFor(t model.Time) string {
	if cfg.Period == 0 { // non-periodic
		return cfg.Prefix
	}
	periodSecs := int64(cfg.Period / time.Second)
	return cfg.tableForPeriod(t.Unix() / periodSecs)
}

func (cfg *PeriodicTableConfig) tableForPeriod(i int64) string {
	return cfg.Prefix + strconv.Itoa(int(i))
}
