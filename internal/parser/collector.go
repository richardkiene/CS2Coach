package parser

import (
	"fmt"
	"log"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/golang/geo/r3"
	dem "github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs"
	common "github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs/events"
	"github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs/msgs2"
	"github.com/richardkiene/CS2Coach/internal/models"
)

type Collector struct {
	tickRate     float64
	tickTime     time.Duration
	mapNameFound bool
	match        *models.Match
	losSystem    *LineOfSightSystem
	parser       dem.Parser
	logger       slog.Logger
	perTickInfo  map[int]map[uint64]PlayerTickData
}

type DamageDealt struct {
	ArmorDamage  int
	HealthDamage int
	HitGroup     byte
}

type PlayerTickData struct {
	SteamID                uint64
	PlayerName             string
	PlayerTeam             common.Team
	Position               r3.Vector
	ViewAngleX             float32
	ViewAngleY             float32
	IsAlive                bool
	Velocity2D             float64
	Velocity3D             float64
	ActiveWeapon           *common.Equipment
	AmmoLeft               [32]int
	EntityID               int
	FlashedAtTick          int
	FlashedTimeRemaining   time.Duration
	Team                   common.Team
	IsAirborne             bool
	IsBlinded              bool
	IsCrouched             bool
	IsConnected            bool
	IsBot                  bool
	IsDefusing             bool
	IsPlanting             bool
	IsReloading            bool
	IsScoped               bool
	IsUpright              bool
	IsWalking              bool
	HasHelmet              bool
	HasKit                 bool
	FiredActiveWeapon      bool
	ArmorRemaining         int
	Assists                int
	Deaths                 int
	Kills                  int
	Health                 int
	Armor                  int
	Damage                 int
	UtilityDamage          int
	Money                  int
	CurrentRoundMoneySpent int
	CurrentMoneySpentTotal int
	DamageDealtToPlayer    map[uint64]DamageDealt
}

type LineOfSightSystem struct {
	mapModel    *Model
	playerModel *Model
	logger      *slog.Logger
}

func NewLineOfSightSystem(mapName, cs2MapsPath string, logger *slog.Logger) (*LineOfSightSystem, error) {
	// HACK REMOVE
	//mapPath := filepath.Join(cs2MapsPath, "maps", mapName+".obj")
	mapPath := "C:\\Users\\richa\\code\\CS2ResourceAPI\\GameDataService\\ModelOutput\\world_output.obj"
	mapModel, err := LoadOBJ(mapPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load map OBJ: %v", err)
	}

	return &LineOfSightSystem{
		mapModel: mapModel,
		logger:   logger,
	}, nil
}

func (los *LineOfSightSystem) LoadPlayerModel() error {
	//HACK REMOVE
	playerModel, err := LoadOBJ("C:\\Users\\richa\\code\\CS2ResourceAPI\\GameDataService\\ModelOutput\\ctm_sas_output.obj")
	if err != nil {
		return fmt.Errorf("failed to load player model: %v", err)
	}
	los.playerModel = playerModel
	return nil
}

func (los *LineOfSightSystem) CanSeeTarget(shooter, target PlayerTickData) bool {
	return CanSeeTarget(shooter, target, los.playerModel, los.mapModel)
}

func (p *PlayerTickData) ForwardVector() r3.Vector {
	// Convert degrees to radians
	yaw := float64(p.ViewAngleX) * (math.Pi / 180)
	pitch := float64(p.ViewAngleY) * (math.Pi / 180)

	// Compute the forward vector components
	forward := r3.Vector{
		X: math.Cos(pitch) * math.Cos(yaw),
		Y: math.Cos(pitch) * math.Sin(yaw),
		Z: -math.Sin(pitch), // Negative because up is usually negative in CS2
	}

	return forward.Normalize() // Ensure it's a unit vector
}

func (p *PlayerTickData) IsInFieldOfView(target r3.Vector) bool {
	const FOV_DEGREES = 90.0 // CS2's typical FOV

	toTarget := r3.Vector{
		X: target.X - p.Position.X,
		Y: target.Y - p.Position.Y,
		Z: target.Z - p.Position.Z,
	}.Normalize()

	forward := p.ForwardVector()
	dotProduct := forward.X*toTarget.X + forward.Y*toTarget.Y + forward.Z*toTarget.Z
	angleRadians := math.Acos(dotProduct)
	angleDegrees := angleRadians * (180 / math.Pi)

	return angleDegrees <= FOV_DEGREES/2
}

func (p PlayerTickData) String() string {
	var team string

	if p.PlayerTeam == 2 {
		team = "Terrorists"
	} else if p.PlayerTeam == 3 {
		team = "Counter-Terrorists"
	} else {
		team = "Unknown"
	}

	return fmt.Sprintf("PlayerTickData{SteamID: %d, Name: %s, Team: %s, Pos: (%.2f, %.2f, %.2f), ViewAngle: (%.2f, %.2f), Alive: %t, Vel2D: %.2f, Vel3D: %.2f}",
		p.SteamID, p.PlayerName, team,
		p.Position.X, p.Position.Y, p.Position.Z,
		p.ViewAngleX, p.ViewAngleY, p.IsAlive,
		p.Velocity2D, p.Velocity3D,
	)
}

func NewCollector(logger *slog.Logger) *Collector {
	return &Collector{
		match:       models.NewMatch(),
		logger:      *logger,
		tickRate:    -1,
		tickTime:    -1,
		perTickInfo: make(map[int]map[uint64]PlayerTickData, 0),
	}
}

func (c *Collector) Collect(demoPath string) (*models.Match, error) {
	f, err := os.Open(demoPath)
	if err != nil {
		return nil, err
	}

	defer f.Close()

	c.parser = dem.NewParser(f)
	defer c.parser.Close()

	c.registerEventHandlers()
	cs2MapsPath, err := c.determineCS2MapsPath()

	if err != nil {
		return nil, err
	}

	// Parse frames until we handle a ServerInfo event with the map name available
	// https://github.com/markus-wa/demoinfocs-golang/issues/435#issuecomment-2613140840
	for !c.mapNameFound {
		moreFrames, err := c.parser.ParseNextFrame()
		if err != nil || !moreFrames {
			if err == dem.ErrUnexpectedEndOfDemo {
				return nil, fmt.Errorf("unable to determine map name from demo file")
			}
			return nil, fmt.Errorf("error during initial parsing: %v", err)
		}
	}

	c.logger.Info("Map name detected: ", "MapName", c.match.MapName)

	losSystem, err := NewLineOfSightSystem(c.match.MapName, cs2MapsPath, &c.logger)
	if err != nil {
		log.Fatalf("Failed to load map data for map %s: %v\n", c.match.MapName, err)
	} else {
		c.losSystem = losSystem
		if err := c.losSystem.LoadPlayerModel(); err != nil {
			return nil, fmt.Errorf("failed to load player model: %v", err)
		}
		c.logger.Debug("Successfully loaded map and player model data", "MapName", c.match.MapName)
	}

	c.logger.Debug("Resuming full parsing...")

	if err := c.parser.ParseToEnd(); err != nil {
		return nil, fmt.Errorf("error during full parsing: %v", err)
	}

	c.logger.Debug("Finished parsing events", "Events", len(c.match.Events))

	for steamID, stats := range c.match.PlayerStats {
		c.logger.Info("Player stats",
			"name", stats.Name,
			"steam_id", steamID,
			"kills", stats.Kills,
			"deaths", stats.Deaths,
			"assists", stats.Assists,
			"total_damage", stats.TotalDamage,
		)
	}

	return c.match, nil
}

func (c *Collector) registerEventHandlers() {
	c.parser.RegisterNetMessageHandler(c.handleServerInfo)
	c.parser.RegisterNetMessageHandler(c.handleEntityUpdate)
	c.parser.RegisterEventHandler(c.handleWeaponFire)
	c.parser.RegisterEventHandler(c.handlePlayerHurt)
}

func (c *Collector) determineCS2MapsPath() (string, error) {
	paths := []string{
		// CS2 paths
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Steam", "steamapps", "common", "Counter-Strike 2", "game", "csgo"),
		filepath.Join(os.Getenv("ProgramFiles"), "Steam", "steamapps", "common", "Counter-Strike 2", "game", "csgo"),
		// CSGO paths
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Steam", "steamapps", "common", "Counter-Strike Global Offensive", "game", "csgo"),
		filepath.Join(os.Getenv("ProgramFiles"), "Steam", "steamapps", "common", "Counter-Strike Global Offensive", "game", "csgo"),
		// Custom Steam library path if set
		filepath.Join(os.Getenv("STEAM_LIBRARY"), "steamapps", "common", "Counter-Strike 2", "game", "csgo"),
		filepath.Join(os.Getenv("STEAM_LIBRARY"), "steamapps", "common", "Counter-Strike Global Offensive", "game", "csgo"),
	}

	for _, path := range paths {
		mapsPath := filepath.Join(path, "maps")
		if fileExists(mapsPath) {
			c.logger.Debug("Found CS2/CSGO installation", "path", path)
			return path, nil
		}
	}

	return "", fmt.Errorf("CS2/CSGO installation not found. Paths checked: %v", paths)
}

func (c *Collector) calculateVelocity3D(currentPos, lastPos r3.Vector) float64 {
	timeDelta := float64(c.tickTime.Milliseconds())

	displacement := currentPos.Sub(lastPos)
	return math.Sqrt(displacement.X*displacement.X+
		displacement.Y*displacement.Y+
		displacement.Z*displacement.Z) / timeDelta
}

func (c *Collector) calculateVelocity2D(currentPos, lastPos r3.Vector) float64 {
	timeDelta := float64(c.tickTime.Milliseconds())

	displacement := currentPos.Sub(lastPos)
	return math.Sqrt(displacement.X*displacement.X+
		displacement.Y*displacement.Y) / timeDelta
}

func (c *Collector) handleServerInfo(msg *msgs2.CSVCMsg_ServerInfo) {
	mapName := msg.GetMapName()
	if !c.mapNameFound && mapName != "" {
		c.match.MapName = mapName
		c.mapNameFound = true
		c.logger.Debug("Map name detected from server info", "MapName", c.match.MapName)
	}
}

func (c *Collector) handleEntityUpdate(msg *msgs2.CSVCMsg_PacketEntities) {
	if c.tickRate == -1 {
		c.tickRate = c.parser.TickRate()
		c.tickTime = c.parser.TickTime()

		c.logger.Debug("Server Tickrate", "tickRate", c.tickRate)
		c.logger.Debug("Tick time", "tickTime", c.tickTime)
	}

	if c.losSystem == nil {
		return
	}

	gs := c.parser.GameState()
	currentTick := gs.IngameTick()

	// At the beginning of the demo, IngameTick can be invalid
	if currentTick < 0 {
		return
	}

	if c.perTickInfo[currentTick] == nil {
		c.perTickInfo[currentTick] = make(map[uint64]PlayerTickData)
	}

	prevTick := currentTick - 1
	var previousTickMap map[uint64]PlayerTickData
	if prevTick >= 0 {
		previousTickMap = c.perTickInfo[prevTick]
	}

	for _, player := range gs.Participants().Playing() {
		if player.SteamID64 == 0 {
			continue
		}

		var lastPlayerTick PlayerTickData
		if previousTickMap != nil {
			if ptd, ok := previousTickMap[player.SteamID64]; ok {
				lastPlayerTick = ptd
			}
		}

		var velocity2D, velocity3D float64

		// We only want to calculate the velocity of a player if they are alive
		if player.IsAlive() {
			velocity2D = c.calculateVelocity2D(player.Position(), lastPlayerTick.Position)
			velocity3D = c.calculateVelocity3D(player.Position(), lastPlayerTick.Position)
		}

		playerTick, found := c.perTickInfo[currentTick][player.SteamID64]
		if !found {
			// Only create a new struct if this is the first time we see this player on this tick
			playerTick = PlayerTickData{
				SteamID:             player.SteamID64,
				DamageDealtToPlayer: make(map[uint64]DamageDealt),
			}
		}

		playerTick.SteamID = player.SteamID64
		playerTick.PlayerTeam = player.Team
		playerTick.PlayerName = player.Name
		playerTick.Position = player.Position()
		playerTick.ViewAngleX = player.ViewDirectionX()
		playerTick.ViewAngleY = player.ViewDirectionY()
		playerTick.IsAlive = player.IsAlive()
		playerTick.Velocity2D = velocity2D
		playerTick.Velocity3D = velocity3D
		playerTick.ActiveWeapon = player.ActiveWeapon()
		playerTick.AmmoLeft = player.AmmoLeft
		playerTick.EntityID = player.Entity.ID()
		playerTick.FlashedAtTick = player.FlashTick
		playerTick.FlashedTimeRemaining = player.FlashDurationTimeRemaining()
		playerTick.Team = player.Team
		playerTick.IsConnected = player.IsConnected
		playerTick.IsAirborne = player.IsAirborne()
		playerTick.IsBlinded = player.IsBlinded()
		playerTick.IsBot = player.IsBot
		playerTick.IsCrouched = player.IsDucking()
		playerTick.IsDefusing = player.IsDefusing
		playerTick.IsPlanting = player.IsPlanting
		playerTick.IsReloading = player.IsReloading
		playerTick.IsScoped = player.IsScoped()
		playerTick.IsUpright = player.IsStanding()
		playerTick.IsWalking = player.IsWalking()
		playerTick.Assists = player.Assists()
		playerTick.Deaths = player.Deaths()
		playerTick.Kills = player.Kills()
		playerTick.Damage = player.TotalDamage()
		playerTick.Health = player.Health()
		playerTick.Armor = player.Armor()
		playerTick.UtilityDamage = player.UtilityDamage()
		playerTick.Money = player.Money()
		playerTick.CurrentRoundMoneySpent = player.MoneySpentThisRound()
		playerTick.CurrentMoneySpentTotal = player.MoneySpentTotal()

		c.perTickInfo[currentTick][player.SteamID64] = playerTick
	}
}

func (c *Collector) handleWeaponFire(e events.WeaponFire) {
	if e.Shooter == nil {
		return
	}

	currentTick := c.parser.GameState().IngameTick()

	if _, exists := c.perTickInfo[currentTick][e.Shooter.SteamID64]; !exists {
		c.perTickInfo[currentTick] = make(map[uint64]PlayerTickData)
	}

	shooterData := c.perTickInfo[currentTick][e.Shooter.SteamID64]
	shooterData.FiredActiveWeapon = true
	c.perTickInfo[currentTick][e.Shooter.SteamID64] = shooterData
}

func (c *Collector) handlePlayerHurt(e events.PlayerHurt) {
	gs := c.parser.GameState()
	currentTick := gs.IngameTick()

	if e.Attacker == nil || e.Player == nil {
		return
	}

	attackerID := e.Attacker.SteamID64
	victimID := e.Player.SteamID64

	if _, exists := c.perTickInfo[currentTick][attackerID]; !exists {
		c.perTickInfo[currentTick] = make(map[uint64]PlayerTickData)
	}

	if _, exists := c.perTickInfo[currentTick][victimID]; !exists {
		c.perTickInfo[currentTick] = make(map[uint64]PlayerTickData)
	}

	attackerData := c.perTickInfo[currentTick][attackerID]
	victimData := c.perTickInfo[currentTick][victimID]

	if attackerData.DamageDealtToPlayer == nil {
		attackerData.DamageDealtToPlayer = make(map[uint64]DamageDealt)
	}

	currentDamage := DamageDealt{
		ArmorDamage:  e.ArmorDamageTaken,
		HealthDamage: e.HealthDamageTaken,
		HitGroup:     byte(e.HitGroup),
	}

	attackerData.DamageDealtToPlayer[victimID] = currentDamage

	c.perTickInfo[currentTick][attackerID] = attackerData
	c.perTickInfo[currentTick][victimID] = victimData
}

func (c *Collector) AnalyzeTimeToDamage() {
	c.logger.Debug("Starting AnalyzeTimeToDamage")
	c.logger.Debug("perTickInfo state", "numTicks", len(c.perTickInfo))

	// Map to store reaction times per player
	// HACK: Just putting this in to get a nice output for now
	reactionTimes := make(map[string][]int64)

	for tick, playerData := range c.perTickInfo {
		for steamID, playerTick := range playerData {
			if !playerTick.IsAlive || playerTick.DamageDealtToPlayer == nil {
				continue
			}

			for victimID, damage := range playerTick.DamageDealtToPlayer {
				if damage.HealthDamage > 0 {
					if playerTick.PlayerName == "shmeeny" {
						c.logger.Debug("DamageDealt",
							"tick", tick,
							"PlayerName", playerTick.PlayerName,
							"PlayerID", steamID,
							"VictimName", c.perTickInfo[tick][victimID].PlayerName,
							"VictimID", victimID,
							"Damage", damage.HealthDamage,
						)
					}
					firstSightTick, exists := c.findLastContinuousVisibilityStart(steamID, victimID, tick)
					if !exists {
						continue
					}

					timeToDamage := int64(tick-firstSightTick) * c.tickTime.Milliseconds()

					if timeToDamage < 0 {
						if playerTick.PlayerName == "shmeeny" {
							continue
						}
					}

					if timeToDamage > 0 && timeToDamage < 1000 {
						// HACK: This is temporary for ouptut testing
						reactionTimes[playerTick.PlayerName] = append(reactionTimes[playerTick.PlayerName], timeToDamage)
						if playerTick.PlayerName == "shmeeny" {
							c.logger.Warn("Player time to damage",
								"player", playerTick.PlayerName,
								"steamID", steamID,
								"reactionTimeMs", timeToDamage,
								"damageAmount", damage.HealthDamage)
						}
					} else {
						continue
					}
				}
			}
		}
	}

	// Compute and log median TTD for each player
	// HACK This is just for testing output for now
	for player, times := range reactionTimes {
		medianTTD := calculateMedian(times)
		c.logger.Warn("Median Time-To-Damage",
			"player", player,
			"medianReactionTimeMs", medianTTD)
	}
}

// Function to calculate median from a slice of int64
// HACK: This needs to live somewhere else
func calculateMedian(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}

	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	mid := len(values) / 2

	if len(values)%2 == 0 {
		return (values[mid-1] + values[mid]) / 2
	}
	return values[mid]
}

func (c *Collector) findLastContinuousVisibilityStart(playerID, targetID uint64, currentTick int) (int, bool) {
	firstSeenTick := -1
	lastSeenTick := -1
	lostVisibilityTick := -1

	for tick := currentTick; tick >= 0; tick-- {
		playerData, exists := c.perTickInfo[tick]
		if !exists {
			continue
		}

		playerTick, exists := playerData[playerID]
		if !exists || !playerTick.IsAlive || playerTick.IsBlinded {
			continue
		}

		targetTick, exists := playerData[targetID]
		if !exists || !targetTick.IsAlive {
			continue
		}

		// EVIL TESTING HACK -- REMOVE ME
		if playerTick.SteamID != 76561197991944713 {
			continue
		}
		// END EVIL TESTING HACK -- REMOVE ME

		isVisible := c.losSystem.CanSeeTarget(playerTick, targetTick)

		/*c.logger.Debug("findLastContinuousVisibilityStart -- Visibility check",
			"tick", tick,
			"player", playerTick.PlayerName,
			"target", targetTick.PlayerName,
			"isVisible", isVisible,
			"forwardVector", playerTick.ForwardVector(),
			"playerPos", playerTick.Position,
			"targetPos", targetTick.Position,
		)*/

		if isVisible {
			if lastSeenTick == -1 { // First tick of seeing the target
				lastSeenTick = tick
			}
			firstSeenTick = tick    // Keep updating first seen tick
			lostVisibilityTick = -1 // Reset lost visibility tracking
		} else {
			if lostVisibilityTick == -1 { // First tick visibility was lost
				lostVisibilityTick = tick
			}
			if lastSeenTick != -1 { // Stop once we find a period where they were seen
				break
			}
		}

		// Ensure we do not force firstSeenTick to be only within the last 128 ticks
		if tick == 0 && firstSeenTick != -1 {
			return firstSeenTick, true
		}
	}

	if firstSeenTick != -1 {
		return firstSeenTick, true
	}

	return 0, false
}
