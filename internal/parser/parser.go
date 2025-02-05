package parser

import (
	"fmt"
	"log"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/golang/geo/r3"
	dem "github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs"
	common "github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs/common"
	events "github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs/events"
	"github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs/msgs2"
	"github.com/richardkiene/CS2Coach/internal/models"
)

type PlayerFrameData struct {
	SteamID      uint64
	PlayerName   string
	PlayerTeam   common.Team
	Position     r3.Vector
	ViewAngleX   float32
	ViewAngleY   float32
	IsAlive      bool
	LastPosition r3.Vector
	Velocity2D   float64
	Velocity3D   float64
	// TODO: Possibly store bounding box corners for finer geometry checks
}

type FrameData struct {
	Tick    int
	Players []PlayerFrameData
}

type FrameStorage struct {
	frames []FrameData
}

type SpottedState struct {
	LastSpottedTime time.Time
	LastVisiblePos  r3.Vector
	ViewAngle       float32
}

type GrenadeData struct {
	Position      r3.Vector
	EqType        common.EquipmentType
	EntityId      int
	DetonateTick  int
	DurationTicks int
}

type Parser struct {
	debug             bool
	match             *models.Match
	parser            dem.Parser
	lastKillTime      *time.Time
	lastKillVictim    uint64
	roundStartTime    time.Time
	sprayStartTime    map[uint64]time.Time
	currentSprayShots map[uint64]int
	// Map of attacker -> (victim -> time that victim was first spotted this round)
	enemySpottedTick       map[uint64]map[uint64]int
	firstDamageTick        map[uint64]map[uint64]int
	alivePlayersByTeam     map[int]int
	currentRoundKills      map[uint64]map[int]int
	lastWeaponFireTime     map[uint64]time.Time
	angleHistory           map[uint64][]float32
	lastKnownHP            map[uint64]int // steamID -> current HP
	lastDamageBy           map[uint64]map[uint64]int
	smokePositions         map[int]GrenadeData // Track active smoke positions
	flashedPlayers         map[uint64]int      // Track the tick when a player is no longer flashed
	frameStorage           FrameStorage
	BspChecker             *BSPVisibilityChecker
	lastProcessedTick      int
	currentTick            int
	visibilityStats        map[uint64]int
	spottedStats           map[uint64]int
	mapNameFound           bool
	sprayThreshold         time.Duration
	playerStatsCache       map[uint64]*models.PlayerStats // Centralized cache for player stats
	lastSprayTick          map[uint64]int                 // Last spray shot tick per player
	sprayTickWindow        int                            // Ticks window for spray (12 ticks = ~200ms)
	visibilityBufferTicks  int
	lastVisibleTarget      map[uint64]uint64
	lastTickVisible        map[uint64]map[uint64]int
	fovDegrees             float64
	engagementTimeoutTicks int
	currentVisibility      map[uint64]map[uint64]bool
	lostVisTick            map[uint64]map[uint64]int // attacker => (victim => tick we lost visibility)
}

func NewParser(debug bool) *Parser {
	return &Parser{
		debug:                  debug,
		match:                  models.NewMatch(),
		lastDamageBy:           make(map[uint64]map[uint64]int),
		alivePlayersByTeam:     make(map[int]int),
		currentRoundKills:      make(map[uint64]map[int]int),
		sprayStartTime:         make(map[uint64]time.Time),
		currentSprayShots:      make(map[uint64]int),
		enemySpottedTick:       make(map[uint64]map[uint64]int),
		firstDamageTick:        make(map[uint64]map[uint64]int),
		lastWeaponFireTime:     make(map[uint64]time.Time),
		angleHistory:           make(map[uint64][]float32),
		lastKnownHP:            make(map[uint64]int),
		visibilityStats:        make(map[uint64]int),
		spottedStats:           make(map[uint64]int),
		flashedPlayers:         make(map[uint64]int),
		lastProcessedTick:      -1,
		frameStorage:           FrameStorage{frames: make([]FrameData, 0)},
		mapNameFound:           false,
		sprayThreshold:         200 * time.Millisecond,
		playerStatsCache:       make(map[uint64]*models.PlayerStats),
		lastSprayTick:          make(map[uint64]int),
		sprayTickWindow:        64, // ~1s at 64 tick
		visibilityBufferTicks:  32, // 32 ticks = ~500ms at 64 tick OLD:320, // 64 ticks = 1 second
		lastVisibleTarget:      make(map[uint64]uint64),
		lastTickVisible:        make(map[uint64]map[uint64]int), // Track the last tick a target was visible to a player
		smokePositions:         make(map[int]GrenadeData),
		fovDegrees:             90,  // 90deg is apparently what we get?
		engagementTimeoutTicks: 256, // ~1s at 64 tick
		currentVisibility:      make(map[uint64]map[uint64]bool),
		lostVisTick:            make(map[uint64]map[uint64]int),
	}
}

func (p *Parser) IsReady() bool {
	return p.BspChecker != nil
}

func (p *Parser) createSmokeGrenadeData(grenadeEvent events.GrenadeEvent) *GrenadeData {
	grenadeData := &GrenadeData{
		Position:     grenadeEvent.Position,
		EqType:       grenadeEvent.GrenadeType,
		EntityId:     grenadeEvent.GrenadeEntityID,
		DetonateTick: p.parser.GameState().IngameTick(),
	}

	return grenadeData
}

func (p *Parser) createFlashGrenadeData(flashEvent events.PlayerFlashed) *GrenadeData {
	grenadeData := &GrenadeData{
		DurationTicks: int(flashEvent.FlashDuration().Seconds()) * 64,
		DetonateTick:  p.parser.GameState().IngameTick(),
	}

	return grenadeData
}

func (p *Parser) GetOrCreatePlayerStats(steamID uint64, name string) *models.PlayerStats {
	if steamID == 0 {
		log.Printf("[ERROR] GetOrCreatePlayerStats called with invalid SteamID: 0")
		debug.PrintStack()
		return nil
	}

	// Check cache first
	if stats, exists := p.playerStatsCache[steamID]; exists {
		// If we didn't previously know the name for this player, we update it when we do
		if stats.Name == "" && name != "" {
			stats.Name = name
			p.playerStatsCache[steamID] = stats
		}
		return stats
	}

	// Fetch or create from match data
	stats := p.match.GetOrCreatePlayerStats(steamID, name)
	if stats == nil {
		log.Printf("[ERROR] Failed to create player stats for SteamID: %d", steamID)
		return nil
	}

	// If we didn't previously know the name for this player, we update it when we do
	if stats.Name == "" && name != "" {
		stats.Name = name
	}

	// Initialize all required maps and slices
	if stats.TimeToFirstDamage == nil {
		stats.TimeToFirstDamage = make([]float64, 0)
	}
	if stats.CrosshairPlacement == nil {
		stats.CrosshairPlacement = make([]float64, 0)
	}
	if stats.Velocity == nil {
		stats.Velocity = make(map[string]float64)
	}
	if stats.MapAreaKills == nil {
		stats.MapAreaKills = make(map[string]int)
	}
	if stats.MapAreaDeaths == nil {
		stats.MapAreaDeaths = make(map[string]int)
	}
	if stats.SurvivalByPhase == nil {
		stats.SurvivalByPhase = make(map[string]int)
	}

	// Cache the stats for future use
	p.playerStatsCache[steamID] = stats

	return stats
}

func (p *Parser) ParseDemo(path string, debug bool) (*models.Match, error) {
	// Open the demo file
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	p.parser = dem.NewParser(f)
	defer p.parser.Close()

	p.match = models.NewMatch()
	p.registerEventHandlers()

	if debug {
		fmt.Println("[DEBUG] Starting demo parse...")
	}

	// Step 1: Parse until the map name is found
	for !p.mapNameFound {
		moreFrames, err := p.parser.ParseNextFrame() // Parse the next frame to process events
		if err != nil || !moreFrames {
			if err == dem.ErrUnexpectedEndOfDemo {
				return nil, fmt.Errorf("unable to determine map name from demo file")
			}
			return nil, fmt.Errorf("error during initial parsing: %v", err)
		}
	}

	if debug {
		fmt.Printf("[DEBUG] Map name detected: %s\n", p.match.MapName)
	}

	// Step 2: Load BSP data
	cs2Path := p.GetCS2Path()
	if cs2Path == "" {
		if debug {
			fmt.Println("[DEBUG] Warning: Could not locate CS2 installation")
		}
		return p.match, nil
	}

	loader := NewBSPLoader(cs2Path, slog.Logger{})
	bspChecker, err := loader.LoadBSPForMap(p.match.MapName)
	if err != nil {
		if debug {
			fmt.Printf("[DEBUG] Warning: Failed to load BSP data for map %s: %v\n", p.match.MapName, err)
		}
		// Continue parsing without BSP data
	} else {
		p.BspChecker = bspChecker
		if debug {
			fmt.Printf("[DEBUG] Successfully loaded BSP data for map %s\n", p.match.MapName)
		}
	}

	// Step 3: Resume parsing events
	if debug {
		fmt.Println("[DEBUG] Resuming full parsing...")
	}
	if err := p.parser.ParseToEnd(); err != nil {
		return nil, fmt.Errorf("error during full parsing: %v", err)
	}

	if debug {
		fmt.Printf("[DEBUG] Finished parsing. Found %d events in total.\n", len(p.match.Events))
	}

	for steamID, stats := range p.match.PlayerStats {
		fmt.Printf("[DEBUG] Player: %s (SteamID: %d), K/D/A: %d/%d/%d, TotalDamage: %d\n",
			stats.Name, steamID, stats.Kills, stats.Deaths, stats.Assists, stats.TotalDamage)
	}

	return p.match, nil
}

func (p *Parser) extractMapName(debug bool) (bool, *models.Match, error) {
	header, err := p.parser.ParseHeader()
	if err != nil {
		return true, nil, fmt.Errorf("failed to parse demo header: %v", err)
	}

	// Attempt to extract map name from the header
	mapName := header.MapName
	if mapName != "" {
		p.match.MapName = mapName
		if debug {
			fmt.Printf("[DEBUG] Map detected from demo header: %s\n", mapName)
		}
	} else if debug {
		fmt.Println("[DEBUG] Map name not found in demo header; will attempt to extract from events.")
	}
	return false, nil, nil
}

func (p *Parser) GetCS2Path() string {
	paths := []string{
		`C:\Program Files (x86)\Steam\steamapps\common\Counter-Strike Global Offensive\game\csgo`,
		`C:\Program Files\Steam\steamapps\common\Counter-Strike Global Offensive\game\csgo`,
	}

	for _, path := range paths {
		if fileExists(filepath.Join(path, "maps")) {
			return path
		}
	}

	return "" // Path not found
}

func (p *Parser) registerEventHandlers() {
	p.parser.RegisterNetMessageHandler(p.handleServerInfo)
	p.parser.RegisterEventHandler(p.handleMatchStart)
	p.parser.RegisterEventHandler(p.handleKill)
	p.parser.RegisterEventHandler(p.handleWeaponFire)
	p.parser.RegisterEventHandler(p.handlePlayerHurt)
	p.parser.RegisterEventHandler(p.handleRoundStart)
	p.parser.RegisterEventHandler(p.handleRoundEnd)
	p.parser.RegisterEventHandler(p.handleTrade)
	p.parser.RegisterEventHandler(p.handleUtility)
	p.parser.RegisterEventHandler(p.handleActivity)
	p.parser.RegisterEventHandler(p.handlePeekTracking)
	//p.parser.RegisterEventHandler(p.handleFrameDone)
	p.parser.RegisterEventHandler(p.handleFlashEvent)
	p.parser.RegisterEventHandler(p.handleSmokeDetonate)
	p.parser.RegisterEventHandler(p.handleSmokeExpire)
	//p.parser.RegisterEventHandler(p.handleGenericEvent) // This is only for raw debugging
	p.parser.RegisterNetMessageHandler(p.handleEntityUpdate)
}

func (p *Parser) handleRoundStart(e events.RoundStart) {
	// Reset round-specific data
	p.roundStartTime = time.Now()
	p.alivePlayersByTeam = make(map[int]int)
	p.currentRoundKills = make(map[uint64]map[int]int)
	p.sprayStartTime = make(map[uint64]time.Time)
	//p.currentSprayShots = make(map[uint64]int)
	p.lastSprayTick = make(map[uint64]int)
	p.enemySpottedTick = make(map[uint64]map[uint64]int)
	p.firstDamageTick = make(map[uint64]map[uint64]int)
	p.lastWeaponFireTime = make(map[uint64]time.Time)
	p.lastDamageBy = make(map[uint64]map[uint64]int)
	p.lastKnownHP = make(map[uint64]int)
	p.enemySpottedTick = make(map[uint64]map[uint64]int)
	p.firstDamageTick = make(map[uint64]map[uint64]int)
	p.flashedPlayers = make(map[uint64]int)
	p.smokePositions = make(map[int]GrenadeData)
	p.lastTickVisible = make(map[uint64]map[uint64]int)
	p.lastVisibleTarget = make(map[uint64]uint64)

	// Update player stats for the new round
	for _, player := range p.parser.GameState().Participants().Playing() {
		if player.SteamID64 == 0 {
			continue
		}

		stats := p.match.GetOrCreatePlayerStats(player.SteamID64, player.Name)
		stats.IsAlive = true
		p.alivePlayersByTeam[int(player.Team)]++

		if p.parser.GameState().IsWarmupPeriod() {
			// Don’t count warmup stats
			stats.TotalDamage = 0
			stats.SurvivalByPhase = make(map[string]int)
			stats.RoundsActive = 0
			stats.Kills = 0
			stats.Deaths = 0
			stats.Assists = 0
			continue
		}

		stats.RoundsActive++
		if p.debug {
			fmt.Printf("[DEBUG] handleRoundStart: Player %s (SteamID: %d), RoundsActive => %d\n",
				player.Name, player.SteamID64, stats.RoundsActive)
		}

		// Initialize HP to 100 for all players
		p.lastKnownHP[player.SteamID64] = 100
	}

	if p.debug {
		fmt.Printf("[DEBUG] Round %d started at tick %d\n",
			p.parser.GameState().TotalRoundsPlayed(),
			p.parser.GameState().IngameTick())
	}

	// Add round_start event to match events
	p.match.AddEvent(models.Event{
		Type: "round_start",
		Data: map[string]interface{}{
			"timestamp":    time.Now().Unix(),
			"round_number": p.parser.GameState().TotalRoundsPlayed(),
		},
	})
}

func (p *Parser) handleFlashEvent(e events.PlayerFlashed) {
	if (!p.isLiveGameRound()) || e.Player == nil {
		return
	}

	currentTick := p.parser.GameState().IngameTick()
	flashDuration := int(e.Player.GetFlashDuration())
	eventDuration := int(e.Player.GetFlashDuration() * 64) // CS2 has 64 ticks per second
	flashEndTick := currentTick + eventDuration

	// New flash event
	if _, exists := p.flashedPlayers[e.Player.SteamID64]; !exists {
		p.flashedPlayers[e.Player.SteamID64] = flashEndTick
		if p.debug {
			fmt.Printf("[VISION HACKING] FlashEvent added for %s -- currentTick: %d flashEndTick: %d flashDurationSeconds: %d eventDuration: %d\n",
				e.Player.Name, currentTick, flashEndTick, flashDuration, eventDuration)
		}
	} else if storedFlashEndTick, exists := p.flashedPlayers[e.Player.SteamID64]; exists {
		// New flash event will go longer than the last
		if storedFlashEndTick < flashEndTick {
			p.flashedPlayers[e.Player.SteamID64] = flashEndTick
			if p.debug {
				fmt.Printf("[VISION HACKING] FlashEvent extended for %s -- currentTick: %d storedFlashEndTick: %d flashEndTick: %d flashDurationSeconds: %d eventDuration: %d\n",
					e.Player.Name, currentTick, storedFlashEndTick, flashEndTick, flashDuration, eventDuration)
			}
		} else {
			if p.debug {
				fmt.Printf("[VISION HACKING] FlashEvent is older than existing flash event for %s -- currentTick: %d storedFlashEndTick: %d flashEndTick: %d\n",
					e.Player.Name, currentTick, storedFlashEndTick, flashEndTick)
			}
		}
	}
}

func (p *Parser) handleServerInfo(msg *msgs2.CSVCMsg_ServerInfo) {
	mapName := msg.GetMapName()
	if !p.mapNameFound && mapName != "" {
		p.match.MapName = mapName
		p.mapNameFound = true
		if p.debug {
			fmt.Printf("[DEBUG] Map name detected from server info: %s\n", p.match.MapName)
		}
	}
}

func (p *Parser) handleMatchStart(e events.MatchStart) {
	p.parser.GameState().TotalRoundsPlayed()
	for _, player := range p.parser.GameState().Participants().Playing() {
		if player.SteamID64 == 0 {
			continue
		}
		stats := p.match.GetOrCreatePlayerStats(player.SteamID64, player.Name)
		stats.Team = int(player.Team)
		stats.IsAlive = true
	}
}

func (p *Parser) handleKill(e events.Kill) {
	if !p.isLiveGameRound() || e.Killer == nil || e.Victim == nil ||
		e.Killer.SteamID64 == 0 || e.Victim.SteamID64 == 0 {
		return
	}

	// Exclude bomb deaths
	if e.Weapon.Type == common.EqBomb {
		return
	}

	killerStats := p.match.GetOrCreatePlayerStats(e.Killer.SteamID64, e.Killer.Name)
	victimStats := p.match.GetOrCreatePlayerStats(e.Victim.SteamID64, e.Victim.Name)

	killerStats.Kills++
	victimStats.Deaths++
	victimStats.IsAlive = false

	// Handle assists
	if damages, exists := p.lastDamageBy[e.Victim.SteamID64]; exists {
		for attackerID, damage := range damages {
			attacker := p.GetOrCreatePlayerStats(attackerID, "")
			if attacker != nil {

				// Only count assists if it isn't self damage and damage is greater than 40
				if attackerID != e.Killer.SteamID64 && damage >= 41 {
					attacker.Assists++
				}
			} else {
				log.Printf("[ERROR] Attacker with ID %d not found!", attackerID)
			}
		}
		delete(p.lastDamageBy, e.Victim.SteamID64)
	}

	// Handle flash assists
	if e.AssistedFlash {
		for _, player := range p.parser.GameState().Participants().Playing() {
			if player.Team != e.Victim.Team && player.FlashDurationTime() > 0 &&
				player.SteamID64 != e.Killer.SteamID64 {
				stats := p.match.GetOrCreatePlayerStats(player.SteamID64, player.Name)
				stats.Assists++
				break
			}
		}
	}

	// Multi-kill tracking
	p.updateMultiKills(e)

	// Update map area stats
	p.updateMapAreaStats(e)
}

func (p *Parser) updateMultiKills(e events.Kill) {
	currentRound := p.parser.GameState().TotalRoundsPlayed()
	if _, exists := p.currentRoundKills[e.Killer.SteamID64]; !exists {
		p.currentRoundKills[e.Killer.SteamID64] = make(map[int]int)
	}
	p.currentRoundKills[e.Killer.SteamID64][currentRound]++

	killerStats := p.match.GetOrCreatePlayerStats(e.Killer.SteamID64, e.Killer.Name)
	if killerStats == nil {
		return
	}

	killCount := p.currentRoundKills[e.Killer.SteamID64][currentRound]
	switch killCount {
	case 2:
		killerStats.TwoKills++
	case 3:
		killerStats.ThreeKills++
	case 4:
		killerStats.FourKills++
	case 5:
		killerStats.FiveKills++
	}
}

func (p *Parser) updateMapAreaStats(e events.Kill) {
	killerStats := p.match.GetOrCreatePlayerStats(e.Killer.SteamID64, e.Killer.Name)
	victimStats := p.match.GetOrCreatePlayerStats(e.Victim.SteamID64, e.Victim.Name)

	if area := getMapArea(Point{
		X: float32(e.Killer.Position().X),
		Y: float32(e.Killer.Position().Y),
		Z: float32(e.Killer.Position().Z),
	}); area != "" {
		killerStats.MapAreaKills[area]++
	}

	if area := getMapArea(Point{
		X: float32(e.Victim.Position().X),
		Y: float32(e.Victim.Position().Y),
		Z: float32(e.Victim.Position().Z),
	}); area != "" {
		victimStats.MapAreaDeaths[area]++
	}
}

/*func (p *Parser) handleGenericEvent(e events.GenericGameEvent) {
	fmt.Printf("handleGenericEvent: e.Name: %s tick: %d\n", e.Name, p.currentTick)
}*/

/*func (p *Parser) handleFrameDone(e events.FrameDone) {
	p.trackPerFramePlayerData(p.parser.GameState())
}*/

func (p *Parser) handleEntityUpdate(msg *msgs2.CSVCMsg_PacketEntities) {
	// fmt.Printf("handleEntityUpdate @ tick: %d\n", p.parser.GameState().IngameTick())

	p.trackPerFramePlayerData(p.parser.GameState())
}

func (p *Parser) trackPerFramePlayerData(gs dem.GameState) {
	currentTick := p.parser.GameState().IngameTick()
	if p.lastProcessedTick < 0 {
		p.lastProcessedTick = currentTick
	}

	if currentTick == p.lastProcessedTick {
		return
	}

	// At the beginning of the demo, IngameTick can be invalid
	if currentTick < 0 {
		return
	}

	frameData := FrameData{
		Tick:    currentTick,
		Players: []PlayerFrameData{},
	}

	// Ensure maps are initialized
	if p.enemySpottedTick == nil {
		p.enemySpottedTick = make(map[uint64]map[uint64]int)
	}
	if p.lostVisTick == nil {
		p.lostVisTick = make(map[uint64]map[uint64]int)
	}

	// Gather current player data
	for _, player := range gs.Participants().Playing() {
		if player.SteamID64 == 0 {
			continue
		}

		pFrame := PlayerFrameData{
			SteamID:    player.SteamID64,
			PlayerTeam: player.Team,
			PlayerName: player.Name,
			Position:   player.Position(),
			ViewAngleX: player.ViewDirectionX(),
			ViewAngleY: player.ViewDirectionY(),
			IsAlive:    player.IsAlive(),
		}
		frameData.Players = append(frameData.Players, pFrame)
	}

	// Only do visibility checks if we have a BspChecker
	if p.BspChecker != nil {
		for i := range frameData.Players {
			obs := frameData.Players[i]
			if !obs.IsAlive {
				continue
			}

			// Ensure submaps exist for this observer
			if _, ok := p.enemySpottedTick[obs.SteamID]; !ok {
				p.enemySpottedTick[obs.SteamID] = make(map[uint64]int)
			}
			if _, ok := p.lastTickVisible[obs.SteamID]; !ok {
				p.lastTickVisible[obs.SteamID] = make(map[uint64]int)
			}
			if _, ok := p.currentVisibility[obs.SteamID]; !ok {
				p.currentVisibility[obs.SteamID] = make(map[uint64]bool)
			}
			if _, ok := p.lostVisTick[obs.SteamID]; !ok {
				p.lostVisTick[obs.SteamID] = make(map[uint64]int)
			}

			for j := range frameData.Players {
				tgt := frameData.Players[j]
				if obs.SteamID == tgt.SteamID || !tgt.IsAlive || obs.PlayerTeam == tgt.PlayerTeam {
					continue
				}

				// 1) Check current & previous visibility
				isVisibleNow := p.rayVisible(obs, tgt)
				wasVisible := p.currentVisibility[obs.SteamID][tgt.SteamID]

				// 2) If we just lost visibility (true->false), record the tick
				if wasVisible && !isVisibleNow {
					p.lostVisTick[obs.SteamID][tgt.SteamID] = currentTick
				}

				// 3) If we just gained visibility (false->true), set earliestSpot if needed
				if !wasVisible && isVisibleNow {
					// If we've never set earliestSpot, do it now
					if _, seen := p.enemySpottedTick[obs.SteamID][tgt.SteamID]; !seen {
						p.enemySpottedTick[obs.SteamID][tgt.SteamID] = currentTick
						if obs.PlayerName == "shmeeny" {
							fmt.Printf("[TTD HACKING] FirstSpot - tick:%d setting spottedTick for %s sees %s\n",
								currentTick, obs.PlayerName, tgt.PlayerName)
						}
					}
					p.lastTickVisible[obs.SteamID][tgt.SteamID] = currentTick
					p.lastVisibleTarget[obs.SteamID] = tgt.SteamID

					// We reacquired visibility, so clear lostVisTick
					delete(p.lostVisTick[obs.SteamID], tgt.SteamID)
				}

				// 4) If still not visible, see how long it's been since we lost sight
				if !isVisibleNow {
					const shortBuffer = 16 // e.g. ~250ms at 64 tick
					lostTick, hasLost := p.lostVisTick[obs.SteamID][tgt.SteamID]
					if hasLost {
						// If we've been invisible for >= shortBuffer ticks, remove earliestSpot
						if currentTick-lostTick >= shortBuffer {
							if _, hasKey := p.enemySpottedTick[obs.SteamID][tgt.SteamID]; hasKey {
								delete(p.enemySpottedTick[obs.SteamID], tgt.SteamID)
								delete(p.lastTickVisible[obs.SteamID], tgt.SteamID)
								delete(p.lastVisibleTarget, obs.SteamID)

								if obs.PlayerName == "shmeeny" {
									fmt.Printf("[TTD HACKING] Removing earliestSpot for %s->%s at tick %d after %d consecutive invisible ticks\n",
										obs.PlayerName, tgt.PlayerName, currentTick, currentTick-lostTick)
								}
							}
						}
					}
				}

				// 5) Update the currentVisibility for next iteration
				p.currentVisibility[obs.SteamID][tgt.SteamID] = isVisibleNow
			}
		}
	}

	p.frameStorage.frames = append(p.frameStorage.frames, frameData)
}

/*
func (p *Parser) trackPerFramePlayerData(gs dem.GameState) {
	p.currentTick = gs.IngameTick()

	// Ensure frame-level processing happens only once per tick
	if p.lastProcessedTick == p.currentTick {
		return
	}
	p.lastProcessedTick = p.currentTick

	currentTime := time.Now()
	timeDelta := currentTime.Sub(p.lastFrameTime)

	frameData := FrameData{
		Tick:          p.currentTick,
		Players:       []PlayerFrameData{},
		VisibilityMap: map[uint64]map[uint64]bool{},
	}

	// Find the previous frame for position comparison
	var lastFrame *FrameData
	if len(p.frameStorage.frames) > 0 {
		lastFrame = &p.frameStorage.frames[len(p.frameStorage.frames)-1]
	}

	// Gather data for each player
	for _, player := range gs.Participants().Playing() {
		if player.SteamID64 == 0 {
			continue
		}

		currentPos := player.Position()
		var velocity2D, velocity3D float64
		var lastPos r3.Vector

		// Get last position from previous frame
		if lastFrame != nil {
			for _, lastPlayer := range lastFrame.Players {
				if lastPlayer.SteamID == player.SteamID64 {
					lastPos = lastPlayer.Position
					velocity2D = p.calculateVelocity2D(currentPos, lastPos, timeDelta)
					velocity3D = p.calculateVelocity3D(currentPos, lastPos, timeDelta)
					break
				}
			}
		}

		if _, exists := p.enemySpottedTime[player.SteamID64]; !exists {
			p.enemySpottedTime[player.SteamID64] = make(map[uint64]int)
		}

		pFrame := PlayerFrameData{
			SteamID:      player.SteamID64,
			PlayerTeam:   player.Team,
			PlayerName:   player.Name,
			Position:     currentPos,
			LastPosition: lastPos,
			ViewAngleX:   player.ViewDirectionX(),
			IsAlive:      player.IsAlive(),
			Velocity2D:   velocity2D,
			Velocity3D:   velocity3D,
		}
		frameData.Players = append(frameData.Players, pFrame)
	}

	// Perform visibility checks only if BSP is loaded
	if p.BspChecker != nil {
		for i := range frameData.Players {
			obs := frameData.Players[i]
			if !obs.IsAlive {
				continue
			}

			if _, ok := frameData.VisibilityMap[obs.SteamID]; !ok {
				frameData.VisibilityMap[obs.SteamID] = map[uint64]bool{}
			}

			for j := range frameData.Players {
				tgt := frameData.Players[j]

				// Skip visibility detection for your own teammates, dead victims, and yourself
				if obs.SteamID == tgt.SteamID || !tgt.IsAlive || obs.PlayerTeam == tgt.PlayerTeam {
					continue
				}

				// Calculate FOV before doing expensive ray tracing
				fov := p.calculateFOV(obs.Position, tgt.Position, obs.ViewAngleX)
				visible := false
				if fov <= 50 && obs.PlayerName == "shmeeny" {
					fmt.Printf("[DEBUG] FOV check: shmeeny -> target %s, FOV: %.2f\n",
						tgt.PlayerName, fov)
				}

				// Same FOV check as rayVisible for consistency -- skip if outside FOV
				if fov <= 50 {
					visible = p.rayVisible(obs, tgt)
				}

				if visible && obs.PlayerName == "shmeeny" {
					fmt.Printf("[DEBUG] Visibility hit: shmeeny can see target %s\n", tgt.PlayerName)
				}

				// Ensure the map for the observer exists
				if _, ok := p.enemySpottedTime[obs.SteamID]; !ok {
					p.enemySpottedTime[obs.SteamID] = make(map[uint64]int)
				}

				const visibilityBufferTicks = 64 // ~1000ms at 64 ticks per second

				if visible {
					// Ensure the map for the observer exists
					if _, ok := p.enemySpottedTime[obs.SteamID]; !ok {
						p.enemySpottedTime[obs.SteamID] = make(map[uint64]int)
					}

					// If the target has just become visible, set the spotted time
					if _, alreadySpotted := p.enemySpottedTime[obs.SteamID][tgt.SteamID]; !alreadySpotted {
						p.enemySpottedTime[obs.SteamID][tgt.SteamID] = p.currentTick
						if obs.PlayerName == "shmeeny" {
							fmt.Printf("[DEBUG] %s spotted %s at tick %d\n", obs.PlayerName, tgt.PlayerName, p.currentTick)
						}
					}

					// Mark the target as visible in the current frame
					if _, ok := p.visibilityCache[obs.SteamID]; !ok {
						p.visibilityCache[obs.SteamID] = make(map[uint64]bool)
					}
					p.visibilityCache[obs.SteamID][tgt.SteamID] = true
				} else {
					// If the target was visible in the last tick but is no longer visible, clear the spotted time
					if prevVisible, exists := p.visibilityCache[obs.SteamID][tgt.SteamID]; exists && prevVisible {
						if p.currentTick-p.enemySpottedTime[obs.SteamID][tgt.SteamID] > visibilityBufferTicks {
							delete(p.enemySpottedTime[obs.SteamID], tgt.SteamID)
							if obs.PlayerName == "shmeeny" {
								fmt.Printf("[DEBUG] %s no longer sees %s at tick %d\n", obs.PlayerName, tgt.PlayerName, p.currentTick)
							}
						}
					}

					// Mark the target as not visible in the current frame
					if _, ok := p.visibilityCache[obs.SteamID]; !ok {
						p.visibilityCache[obs.SteamID] = make(map[uint64]bool)
					}
					p.visibilityCache[obs.SteamID][tgt.SteamID] = false
				}

				// Always update the visibility map
				frameData.VisibilityMap[obs.SteamID][tgt.SteamID] = visible
			}
		}
	} else if p.debug {
		fmt.Printf("[DEBUG] BSPChecker not initialized; skipping visibility checks for tick %d\n", p.currentTick)
	}

	// Store frameData in p.frameStorage
	p.frameStorage.frames = append(p.frameStorage.frames, frameData)
}
*/

func (p *Parser) calculateVelocity3D(currentPos, lastPos r3.Vector, timeDelta time.Duration) float64 {
	if timeDelta == 0 {
		return 0
	}

	displacement := currentPos.Sub(lastPos)
	return math.Sqrt(displacement.X*displacement.X+
		displacement.Y*displacement.Y+
		displacement.Z*displacement.Z) / timeDelta.Seconds()
}

func (p *Parser) calculateVelocity2D(currentPos, lastPos r3.Vector, timeDelta time.Duration) float64 {
	if timeDelta == 0 {
		return 0
	}

	displacement := currentPos.Sub(lastPos)
	return math.Sqrt(displacement.X*displacement.X+
		displacement.Y*displacement.Y) / timeDelta.Seconds()
}

func (p *Parser) InitializeParser(demoFile string, debug bool) error {
	// Load the BSP data for the map
	if err := p.loadBspData(debug); err != nil {
		return fmt.Errorf("failed to load BSP data: %w", err)
	}

	fmt.Println("[INFO] BSP data loaded successfully.")
	return nil
}

func (p *Parser) loadBspData(debug bool) error {
	_, _, mapName := p.extractMapName(debug) // Assuming you have a method to get the map name
	bspPath := fmt.Sprintf("maps/%s", mapName)

	// Initialize the BSP checker
	var err error
	p.BspChecker, err = NewBSPVisibilityChecker(bspPath, "", slog.Logger{}) // Replace with your BSP loader logic
	if err != nil {
		return err
	}
	return nil
}

func (p *Parser) rayVisible(obs PlayerFrameData, tgt PlayerFrameData) bool {
	/*
	 * Important: The order of checks in this function matter.
	 * Re-ordering checks will cause issues!
	 */

	// Early distance check at 1500 units
	direction := tgt.Position.Sub(obs.Position)
	distance := direction.Norm()
	if distance > 1500 {
		return false
	}

	// Map visibility check
	// comment out because broken by updates
	/*if p.BspChecker != nil && !p.BspChecker.IsVisible(obs.Position, tgt.Position) {
		return false
	}*/

	// FOV check only - remove the redundant angle checks
	horizontalFOV := p.calculateFOV(obs.Position, tgt.Position, obs.ViewAngleX)
	verticalFOV := p.calculateVerticalFOV(obs.Position, tgt.Position, obs.ViewAngleY)
	if horizontalFOV > p.fovDegrees || verticalFOV > p.fovDegrees {
		return false
	}

	/*if fov <= 50 && obs.PlayerName == "shmeeny" {
		fmt.Printf("[DEBUG] FOV check: shmeeny -> target %d, FOV: %.2f\n", tgt.SteamID, fov)
	}*/

	// Only validate smoke and flash if FOV check passes
	if p.isLineInSmoke(obs.Position, tgt.Position) {
		if p.debug && obs.PlayerName == "shmeeny" {
			fmt.Printf("[VISION HACKING] Smoke blocked vision for shmeeny at tick %d\n", p.parser.GameState().IngameTick())
		}
		return false
	}

	if p.isPlayerFlashed(obs.SteamID) {
		if p.debug && obs.PlayerName == "shmeeny" {
			fmt.Printf("[VISION HACKING] Flash blocked vision for shmeeny at tick %d\n", p.parser.GameState().IngameTick())
		}
		return false
	}

	return true
}

func (p *Parser) calculateVerticalFOV(src, dst r3.Vector, viewAngleY float32) float64 {
	deltaZ := dst.Z - src.Z
	horizontalDist := math.Sqrt(math.Pow(dst.X-src.X, 2) + math.Pow(dst.Y-src.Y, 2))
	angleRad := math.Atan2(float64(deltaZ), float64(horizontalDist))
	angleDeg := angleRad * 180.0 / math.Pi
	return math.Abs(float64(viewAngleY) - angleDeg)
}

func (p *Parser) calculateFOV(src, dst r3.Vector, viewAngle float32) float64 {
	targetAngle := calcAngleBetween(src, dst)
	deltaAngle := float64(targetAngle - viewAngle)

	// Normalize delta angle
	for deltaAngle > 180 {
		deltaAngle -= 360
	}
	for deltaAngle < -180 {
		deltaAngle += 360
	}

	return math.Abs(deltaAngle)
}

func calcAngleBetween(from, to r3.Vector) float32 {
	deltaX := to.X - from.X
	deltaY := to.Y - from.Y

	angleRad := math.Atan2(float64(deltaY), float64(deltaX))
	angleDeg := angleRad * 180.0 / math.Pi
	return float32(angleDeg)
}

func getUtilityValue(grenadeType common.EquipmentType) int {
	switch grenadeType {
	case common.EqHE:
		return 300
	case common.EqFlash:
		return 200
	case common.EqSmoke:
		return 300
	case common.EqMolotov:
		return 400
	case common.EqIncendiary:
		return 600
	default:
		return 0
	}
}

func (p *Parser) handleSmokeDetonate(e events.SmokeStart) {
	if !p.isLiveGameRound() {
		return
	}

	// Findout if this is a duplicate event we can ignore
	if _, exists := p.smokePositions[e.GrenadeEvent.GrenadeEntityID]; !exists {
		detonatedSmoke := *p.createSmokeGrenadeData(e.GrenadeEvent)
		p.smokePositions[e.GrenadeEvent.GrenadeEntityID] = detonatedSmoke
		if p.debug {
			fmt.Printf("[VISION HACKING] Adding detonated smoke -- Tick: %d EntityID: %d\n", detonatedSmoke.DetonateTick, detonatedSmoke.EntityId)
		}
	} else {
		if p.debug {
			fmt.Printf("[VISION HACKING] Skipping duplicate SmokeStart Event -- EntityID: %d\n", e.GrenadeEvent.GrenadeEntityID)
		}
	}
}

func (p *Parser) handleSmokeExpire(e events.SmokeExpired) {
	if !p.isLiveGameRound() {
		return
	}

	if detonatedSmoke, exists := p.smokePositions[e.GrenadeEvent.GrenadeEntityID]; exists {
		delete(p.smokePositions, detonatedSmoke.EntityId)
		if p.debug {
			fmt.Printf("[VISION HACKING] Expired smoke -- DetonatedTick: %d CurrentTick: %d EntityID: %d\n", detonatedSmoke.DetonateTick, p.parser.GameState().IngameTick(), detonatedSmoke.EntityId)
		}
	} else {
		if p.debug {
			fmt.Printf("[VISION HACKING] No smoke exists to expire -- EntityID: %d\n", e.GrenadeEvent.GrenadeEntityID)
		}
	}
}

func (p *Parser) handleUtility(e events.GrenadeEvent) {
	if (!p.isLiveGameRound()) || e.Thrower == nil {
		return
	}

	stats := p.match.GetOrCreatePlayerStats(e.Thrower.SteamID64, e.Thrower.Name)

	switch e.Grenade.Type {
	case common.EqFlash:
		stats.UtilityStats.FlashesThrown++
		for _, player := range p.parser.GameState().Participants().Playing() {
			if player.FlashDurationTime() > 0 {
				if player.Team == e.Thrower.Team {
					stats.UtilityStats.TeammatesFlashed++
				} else {
					stats.UtilityStats.EnemiesFlashed++
					stats.UtilityStats.TotalBlindDuration += player.FlashDurationTime().Seconds()
				}
			}
		}
	case common.EqHE:
		stats.UtilityStats.HEGrenadesThrown++
	case common.EqSmoke:
		stats.UtilityStats.SmokesThrown++
	case common.EqMolotov, common.EqIncendiary:
		stats.UtilityStats.MolotovsThrown++
	}

	stats.UnusedUtilityValue += getUtilityValue(e.Grenade.Type)
}

func (p *Parser) handleTrade(e events.Kill) {
	if !p.isLiveGameRound() || e.Killer == nil || e.Victim == nil ||
		e.Killer.SteamID64 == 0 || e.Victim.SteamID64 == 0 {
		return
	}

	// Record trade opportunities and attempts
	for _, player := range p.parser.GameState().Participants().Playing() {
		if player.Team == e.Victim.Team && player.IsAlive() {
			stats := p.match.GetOrCreatePlayerStats(player.SteamID64, player.Name)
			if stats == nil {
				continue
			}

			stats.TradeKillOpportunities++

			lastFireTime, exists := p.lastWeaponFireTime[player.SteamID64]
			if exists && time.Since(lastFireTime) <= 3*time.Second {
				stats.TradeKillAttempts++
			}
		}
	}

	// Record traded deaths
	victim := p.GetOrCreatePlayerStats(e.Victim.SteamID64, e.Victim.Name)
	if victim == nil {
		return
	}

	victim.TradedDeathOpportunities++

	lastFireTime, exists := p.lastWeaponFireTime[e.Victim.SteamID64]
	if exists && time.Since(lastFireTime) <= 3*time.Second {
		victim.TradedDeathAttempts++

		if p.lastKillTime != nil && time.Since(*p.lastKillTime) <= 3*time.Second {
			if p.lastKillVictim == e.Killer.SteamID64 {
				victim.TradedDeaths++
			}
		}
	}
}

// handleActivity is primarily for utility damage, but keep it separate if you plan to expand it
func (p *Parser) handleActivity(e events.PlayerHurt) {
	if !p.isLiveGameRound() || e.Attacker == nil || e.Player == nil ||
		e.Attacker.SteamID64 == 0 || e.Player.SteamID64 == 0 {
		return
	}

	stats := p.match.GetOrCreatePlayerStats(e.Attacker.SteamID64, e.Attacker.Name)

	switch e.Weapon.Type {
	case common.EqHE:
		stats.UtilityStats.HEDamage += e.HealthDamage
	case common.EqMolotov, common.EqIncendiary:
		if e.Attacker.Team == e.Player.Team {
			stats.TeamUtilityDamage += e.HealthDamage
		} else {
			stats.UtilityStats.MolotovsThrown++
			// TODO: track molly damage
		}
	}
}

// Check if the weapon is a rifle
func (p *Parser) isRifle(weaponClass common.EquipmentClass) bool {
	switch weaponClass {
	case common.EqClassRifle:
		return true
	default:
		return false
	}
}

// Get the max movement speed for the given weapon
func (p *Parser) getWeaponMaxSpeed(weaponType common.EquipmentType) float64 {
	switch weaponType {
	case common.EqAK47, common.EqGalil:
		return 215.0
	case common.EqM4A1, common.EqM4A4:
		return 225.0
	case common.EqFamas, common.EqAUG:
		return 220.0
	default:
		return 250.0 // Default fallback for unknown weapon types implement https://www.reddit.com/r/GlobalOffensive/comments/a28h8r/movement_speed_chart/
	}
}

func (p *Parser) handleWeaponFire(e events.WeaponFire) {
	if !p.isLiveGameRound() || e.Shooter == nil || e.Shooter.SteamID64 == 0 || e.Weapon.Class() == common.EqClassGrenade || e.Weapon.Type == common.EqBomb {
		return
	}

	// Update the current tick
	p.currentTick = p.parser.GameState().IngameTick()

	stats := p.match.GetOrCreatePlayerStats(e.Shooter.SteamID64, e.Shooter.Name)
	if stats == nil {
		return
	}

	stats.ShotsTotal++

	var currentVelocity float64
	for _, player := range p.frameStorage.frames[len(p.frameStorage.frames)-1].Players {
		if player.SteamID == e.Shooter.SteamID64 {
			// TODO: Evaluate using Velocity3D here
			currentVelocity = player.Velocity2D
			break
		}
	}

	shooterID := e.Shooter.SteamID64
	currentTime := time.Now()
	enemySpotted := false

	if victimID, exists := p.lastVisibleTarget[shooterID]; exists {
		if e.Shooter.Name == "shmeeny" && p.debug {
			log.Printf("[SPOTTED HACKING] lastVisibleTarget[%d]: %d", e.Shooter.SteamID64, p.lastVisibleTarget[e.Shooter.SteamID64])
		}
		if spottedTick, wasSpotted := p.enemySpottedTick[shooterID][victimID]; wasSpotted {
			if e.Shooter.Name == "shmeeny" && p.debug {
				log.Printf("[SPOTTED HACKING] enemySpottedTime[%d][%d]: %d", e.Shooter.SteamID64, victimID, p.enemySpottedTick[e.Shooter.SteamID64][victimID])
			}
			ticksSinceSpotted := p.parser.GameState().IngameTick() - spottedTick
			if ticksSinceSpotted >= 0 && ticksSinceSpotted <= p.visibilityBufferTicks {
				stats.EnemySpottedShots++
				enemySpotted = true

				if e.Shooter.Name == "shmeeny" && p.debug {
					fmt.Printf("[SPOTTED HACKING] shmeeny's EnemySpottedShots incremented to %d shots at %d ticksSinceSpotted: %d spottedTick: %d currentTick: %d\n",
						stats.EnemySpottedShots, victimID, ticksSinceSpotted, spottedTick, p.parser.GameState().IngameTick())
				}

				if p.debug {
					fmt.Printf("[DEBUG] EnemySpottedShots incremented: %s shot at %d (ticksSinceSpotted: %d)\n",
						e.Shooter.Name, victimID, ticksSinceSpotted)
				}
			} else if e.Shooter.Name == "shmeeny" && p.debug {
				fmt.Printf("[SPOTTED HACKING] Skipped EnemySpottedShots: %s shot at %d (ticksSinceSpotted: %d > visibilityBufferTicks: %d)\n",
					e.Shooter.Name, victimID, ticksSinceSpotted, p.visibilityBufferTicks)
			}
		}
	} else if e.Shooter.Name == "shmeeny" && p.debug {
		fmt.Printf("[SPOTTED HACKING] No last visible target for shooter %s\n", e.Shooter.Name)
	}

	// Track spray timing
	if p.isSprayableWeapon(e.Weapon.Class()) {
		if p.debug {
			fmt.Printf("[DEBUG] Weapon %s classified as sprayable for %s\n", e.Weapon.Type, e.Shooter.Name)
		}

		if _, exists := p.currentSprayShots[shooterID]; !exists {
			p.currentSprayShots[shooterID] = 0
			p.lastSprayTick[shooterID] = p.currentTick
			if p.debug {
				fmt.Printf("[DEBUG] Initializing spray tracking for %s\n", e.Shooter.Name)
			}
		}

		// Check if the spray is within the defined tick window
		if p.currentTick-p.lastSprayTick[shooterID] <= p.sprayTickWindow {
			if enemySpotted {
				p.currentSprayShots[shooterID]++
				stats.SprayShots++

				if p.debug {
					fmt.Printf("[DEBUG] Spray shot added for %s. SprayShots: %d\n", e.Shooter.Name, stats.SprayShots)
				}
			}
		} else {
			// Only reset if we've exceeded the window significantly
			if p.currentTick-p.lastSprayTick[shooterID] > p.sprayTickWindow*2 {
				p.currentSprayShots[shooterID] = 1
			}
		}
		p.lastSprayTick[shooterID] = p.currentTick
	}

	// Handle rifle shots and counter-strafing
	if p.isRifle(e.Weapon.Class()) {
		// Track moving rifle shots
		if currentVelocity > 0.0 {
			stats.MovingRifleShots++
			if p.debug {
				fmt.Printf("[DEBUG] Player %s fired a moving rifle shot. MovingRifleShots: %d\n",
					e.Shooter.Name, stats.MovingRifleShots)
			}
		}

		// Counter-strafing detection: velocity < 34% of weapon's max speed
		maxSpeed := p.getWeaponMaxSpeed(e.Weapon.Type)
		if currentVelocity > 0.0 && currentVelocity < 0.34*maxSpeed && !e.Shooter.IsDucking() && enemySpotted {
			stats.CounterStrafedShots++
			if p.debug {
				fmt.Printf("[DEBUG] Counter-strafe detected for %s: velocity=%.2f, maxSpeed=%.2f\n",
					e.Shooter.Name, currentVelocity, maxSpeed)
			}
		} else if p.debug {
			if !enemySpotted {
				fmt.Printf("[DEBUG] Counter-strafe skipped for %s: No spotted enemy.\n", e.Shooter.Name)
			}
			if currentVelocity == 0 {
				fmt.Printf("[DEBUG] Counter-strafe skipped for %s: Player is stationary.\n", e.Shooter.Name)
			}
			if currentVelocity >= 0.34*maxSpeed {
				fmt.Printf("[DEBUG] Counter-strafe skipped for %s: Velocity too high (%.2f > %.2f).\n",
					e.Shooter.Name, currentVelocity, 0.34*maxSpeed)
			}
			if e.Shooter.IsDucking() {
				fmt.Printf("[DEBUG] Counter-strafe skipped for %s: Player is crouching.\n", e.Shooter.Name)
			}
		}
	}

	// Store velocity for debugging/analysis
	stats.Velocity[e.Shooter.Name] = currentVelocity
	p.lastWeaponFireTime[shooterID] = currentTime
}

func (p *Parser) isLiveGameRound() bool {
	gs := p.parser.GameState()
	return !gs.IsWarmupPeriod() && !gs.IsFreezetimePeriod() && gs.TotalRoundsPlayed() >= 0
}

func (p *Parser) isSprayableWeapon(eq common.EquipmentClass) bool {
	return eq == common.EqClassRifle || eq == common.EqClassSMG
}

func (p *Parser) handlePlayerHurt(e events.PlayerHurt) {
	if !p.isLiveGameRound() || e.Attacker == nil || e.Player == nil ||
		e.Attacker.SteamID64 == 0 || e.Player.SteamID64 == 0 {
		return
	}

	stats := p.match.GetOrCreatePlayerStats(e.Attacker.SteamID64, e.Attacker.Name)
	if stats == nil {
		return
	}

	victimID := e.Player.SteamID64

	// Hits should not count if it was a grenade, bomb, or team/self damage
	if e.Weapon.Class() != common.EqClassGrenade && e.Attacker.Team != e.Player.Team && e.Attacker.SteamID64 != e.Player.SteamID64 && e.Weapon.Type != common.EqBomb {
		// Accuracy (All) hit counts
		stats.HitsTotal++

		if e.Attacker.Name == "shmeeny" {
			fmt.Printf("[TTD HACKING] Increment HitsTotal for shmeeny -- Current Tick:%d Victim:%s\n", p.parser.GameState().IngameTick(), e.Player.Name)
		}

		// Headshot Stats counts
		if e.HitGroup == events.HitGroupHead {
			stats.Headshots++
		}

		// Time To Damage tracking
		if lastSeenID, exists := p.lastVisibleTarget[e.Attacker.SteamID64]; exists && lastSeenID == victimID {
			if _, exists := p.firstDamageTick[e.Attacker.SteamID64]; !exists {
				p.firstDamageTick[e.Attacker.SteamID64] = make(map[uint64]int)
			}
			if spottedTick, exists := p.enemySpottedTick[e.Attacker.SteamID64][victimID]; exists {
				// They have a valid earliest sighting for this attacker->victim
				if _, alreadyCounted := p.firstDamageTick[e.Attacker.SteamID64][victimID]; !alreadyCounted {
					currentTick := p.parser.GameState().IngameTick()
					ticksSinceSpotted := currentTick - spottedTick
					timeToFirstDamageMs := float64(ticksSinceSpotted) * 15.625 // ~ ms at 64 tick or (1000/64) = 15.625, whichever approach you prefer.

					/* Exclude if TTD >= 1000ms (i.e., >= 1 second)
					 * See: https://leetify.com/blog/leetify-stats-glossary/
					 * "We measure the time it takes from the point of you first seeing the enemy to the point where you first dealt damage to them.
					 * Any Time to Damage of 1s+ is excluded to account for trigger discipline plays. We then use the median to exclude outliers."
					 */
					if timeToFirstDamageMs < 1000 {
						stats.TimeToFirstDamage = append(stats.TimeToFirstDamage, timeToFirstDamageMs)
					}

					// Mark that we counted the first damage
					p.firstDamageTick[e.Attacker.SteamID64][victimID] = currentTick

					if e.Attacker.Name == "shmeeny" {
						fmt.Printf("[TTD HACKING] earliestSpot:%d, currentTick:%d, ticksSinceSpotted:%d, TTDms:%.2f, (excluded if >=1000)\n",
							spottedTick, currentTick, ticksSinceSpotted, timeToFirstDamageMs)
					}
				}
			}
		}
	}

	victimStats := p.GetOrCreatePlayerStats(e.Player.SteamID64, e.Player.Name)
	if victimStats == nil {
		return
	}

	oldHP := p.lastKnownHP[victimID]
	actualDamage := min(e.HealthDamage, oldHP)

	// Initialize damage tracking maps if needed
	if _, exists := p.lastDamageBy[victimID]; !exists {
		p.lastDamageBy[victimID] = make(map[uint64]int)
	}

	// Count as total damage only if not team damage or self-inflicted and not from the bomb
	if e.Attacker.Team != e.Player.Team && e.Attacker.SteamID64 != e.Player.SteamID64 && e.Weapon.Type != common.EqBomb {
		stats.TotalDamage += actualDamage

		p.lastDamageBy[victimStats.SteamID][e.Attacker.SteamID64] += actualDamage

		// Check if this hit was part of a spray
		if p.isSprayableWeapon(e.Weapon.Class()) {
			attackerID := e.Attacker.SteamID64
			if count, exists := p.currentSprayShots[attackerID]; exists && count >= 3 {
				stats.SprayHits++
				if p.debug {
					fmt.Printf("[DEBUG] Spray hit by %s (shots: %d hits: %d total: %d)\n",
						e.Attacker.Name, p.currentSprayShots[attackerID], stats.SprayHits, stats.SprayShots)
				}
			}
		}

		// Optional debug logging, to confirm it’s adding up:
		if p.debug && e.Player.Name == "shmeeny" {
			fmt.Printf("[DEBUG] p.lastDamageBy[%d][%d] is now %d\n",
				victimID, e.Attacker.SteamID64,
				p.lastDamageBy[victimID][e.Attacker.SteamID64])
		}
		if p.debug && e.Player.Name == "shmeeny" {
			fmt.Printf("[DEBUG] Player %s dealt %d damage to %s. TotalDamage: %d\n",
				e.Attacker.Name, actualDamage, e.Player.Name, stats.TotalDamage)
		}

		p.lastKnownHP[victimID] = max(oldHP-actualDamage, 0)
	}
}

/*func (p *Parser) handlePlayerHurt(e events.PlayerHurt) {
	if !p.isLiveGameRound() || e.Attacker == nil || e.Player == nil ||
		e.Attacker.SteamID64 == 0 || e.Player.SteamID64 == 0 {
		return
	}

	stats := p.match.GetOrCreatePlayerStats(e.Attacker.SteamID64, e.Attacker.Name)
	if stats == nil {
		return
	}

	stats.HitsTotal++

	victimStats := p.GetOrCreatePlayerStats(e.Player.SteamID64, e.Player.Name)
	if victimStats == nil {
		return
	}

	if e.HitGroup == events.HitGroupHead {
		stats.Headshots++
	}

	victimID := e.Player.SteamID64
	oldHP := p.lastKnownHP[victimID]
	actualDamage := min(e.HealthDamage, oldHP)

	// Initialize damage tracking maps if needed
	if _, exists := p.lastDamageBy[victimID]; !exists {
		p.lastDamageBy[victimID] = make(map[uint64]int)
	}

	// Count as total damage only if not team damage or self-inflicted
	if e.Attacker.Team != e.Player.Team && e.Attacker.SteamID64 != e.Player.SteamID64 {
		stats.TotalDamage += actualDamage

		p.lastDamageBy[victimStats.SteamID][e.Attacker.SteamID64] += actualDamage

		// Check if this hit was part of a spray
		if p.isSprayableWeapon(e.Weapon.Class()) {
			attackerID := e.Attacker.SteamID64
			if count, exists := p.currentSprayShots[attackerID]; exists && count >= 3 {
				stats.SprayHits++
				if p.debug {
					fmt.Printf("[DEBUG] Spray hit by %s (shots: %d hits: %d total: %d)\n",
						e.Attacker.Name, p.currentSprayShots[attackerID], stats.SprayHits, stats.SprayShots)
				}
			}
		}

		// Optional debug logging, to confirm it’s adding up:
		if p.debug && e.Player.Name == "shmeeny" {
			fmt.Printf("[DEBUG] p.lastDamageBy[%d][%d] is now %d\n",
				victimID, e.Attacker.SteamID64,
				p.lastDamageBy[victimID][e.Attacker.SteamID64])
		}
		if p.debug && e.Player.Name == "shmeeny" {
			fmt.Printf("[DEBUG] Player %s dealt %d damage to %s. TotalDamage: %d\n",
				e.Attacker.Name, actualDamage, e.Player.Name, stats.TotalDamage)
		}
	}

	p.lastKnownHP[victimID] = max(oldHP-actualDamage, 0)

	// Check if victim was previously spotted
	if p.BspChecker != nil {
		if spottedTick, wasSpotted := p.enemySpottedTime[e.Attacker.SteamID64][victimID]; wasSpotted {
			if _, exists := p.firstDamageTime[e.Attacker.SteamID64]; !exists {
				p.firstDamageTime[e.Attacker.SteamID64] = make(map[uint64]int)
			}

			// Check if damage has already been counted
			if _, alreadyCounted := p.firstDamageTime[e.Attacker.SteamID64][victimID]; !alreadyCounted {
				stats.EnemySpottedHits++
				if p.debug && e.Attacker.Name == "shmeeny" {
					fmt.Printf("[DEBUG] handlePlayerHurt: %s => EnemySpottedHits incremented to %d\n",
						e.Attacker.Name, stats.EnemySpottedHits)
				}

				// Track ticks to first damage
				ticksToHit := p.currentTick - spottedTick
				timeToFirstDamage := float64(ticksToHit) / 64.0 * 1000 // Convert ticks to milliseconds

				if timeToFirstDamage <= 1000 { // Exclude values > 1 second (trigger discipline)
					stats.TimeToFirstDamage = append(stats.TimeToFirstDamage, timeToFirstDamage)
				}

				// Update firstDamageTime
				if _, exists := p.firstDamageTime[e.Attacker.SteamID64]; !exists {
					p.firstDamageTime[e.Attacker.SteamID64] = make(map[uint64]int)
				}
				p.firstDamageTime[e.Attacker.SteamID64][victimID] = p.currentTick

				if p.debug && e.Attacker.Name == "shmeeny" {
					fmt.Printf("[DEBUG] Attacker %s saw victim %s for %d ticks (%.2fms) before hitting.\n",
						e.Attacker.Name, e.Player.Name, ticksToHit, timeToFirstDamage)
				}
			}
		}
	} else if p.debug && !p.warnedBspChecker {
		fmt.Println("[DEBUG] Warning: bspChecker is nil, skipping visibility checks for damage events.")
		p.warnedBspChecker = true
	}
}*/

func (p *Parser) handleRoundEnd(e events.RoundEnd) {
	for _, player := range p.parser.GameState().Participants().Playing() {
		if player.SteamID64 == 0 {
			continue
		}
		stats := p.match.GetOrCreatePlayerStats(player.SteamID64, player.Name)

		if p.debug {
			fmt.Printf("[DEBUG] End of round stats for %s: K/D/A: %d/%d/%d, TotalDamage: %d\n",
				stats.Name, stats.Kills, stats.Deaths, stats.Assists, stats.TotalDamage)

			if stats.SprayShots > 0 {
				sprayAccuracy := float64(stats.SprayHits) / float64(stats.SprayShots) * 100
				fmt.Printf("[DEBUG] Player %s Spray Accuracy: %.2f%%\n", stats.Name, sprayAccuracy)
			} else {
				fmt.Printf("[DEBUG] Player %s Spray Accuracy: No spray shots recorded\n", stats.Name)
			}
		}

		if stats.IsAlive {
			stats.RoundsSurvived++

			timeInRound := time.Since(p.roundStartTime).Seconds()
			switch {
			case timeInRound < 30:
				stats.SurvivalByPhase["early"]++
			case timeInRound < 60:
				stats.SurvivalByPhase["mid"]++
			default:
				stats.SurvivalByPhase["late"]++
			}
		}

		// HACK commented out to keep the complier happy
		//stats.MedianTTD = calculateMedian(stats.TimeToFirstDamage)
	}

	if p.debug {
		fmt.Printf("[DEBUG] Round ended -- winner: %d reason: %d\n", e.Winner, e.Reason)
	}

	p.match.AddEvent(models.Event{
		Type: "round_end",
		Data: map[string]interface{}{
			"winner":    e.Winner,
			"reason":    e.Reason,
			"timestamp": time.Now().Unix(),
		},
	})
}

func (p *Parser) handlePeekTracking(e events.Kill) {
	if !p.isLiveGameRound() || e.Killer == nil || e.Victim == nil ||
		e.Killer.SteamID64 == 0 || e.Victim.SteamID64 == 0 {
		return
	}

	killerStats := p.match.GetOrCreatePlayerStats(e.Killer.SteamID64, e.Killer.Name)

	killerPos := e.Killer.Position()
	victimPos := e.Victim.Position()
	killerAngle := e.Killer.ViewDirectionX()

	if isQuickPeek(killerPos, victimPos, killerAngle) {
		killerStats.PeekKills++
	}
}

func isQuickPeek(killerPos, victimPos r3.Vector, killerAngle float32) bool {
	deltaX := victimPos.X - killerPos.X
	deltaY := victimPos.Y - killerPos.Y

	angle := float32(math.Atan2(float64(deltaY), float64(deltaX))) * 180 / math.Pi
	angleDiff := math.Abs(float64(angle - killerAngle))
	return angleDiff <= 45
}

type Point struct {
	X, Y, Z float32
}

func getMapArea(pos Point) string {
	if pos.Z > 200 {
		return "upper"
	} else if pos.Z < -200 {
		return "lower"
	}
	return "mid"
}

func (p *Parser) isPlayerFlashed(playerID uint64) bool {
	if flashEndTick, exists := p.flashedPlayers[playerID]; exists {
		// If now is *before* flashEndTick, the player is still flashed
		isFlashed := p.parser.GameState().IngameTick() < flashEndTick
		if isFlashed && p.debug {
			fmt.Printf("[VISION HACKING] PlayerID: %d is flashed. Current Tick: %d flashEndTick: %d\n", playerID, p.parser.GameState().IngameTick(), flashEndTick)
		}
		return isFlashed
	}
	return false
}

func (p *Parser) isLineInSmoke(start, end r3.Vector) bool {
	const (
		smokeRadius = 180.0
		bloomTicks  = 64 // ~1 second at 64 tick
		segments    = 4
	)

	for _, smoke := range p.smokePositions {
		// Skip smoke if it hasn't bloomed yet
		ticksSinceDetonate := p.currentTick - smoke.DetonateTick
		if ticksSinceDetonate < bloomTicks {
			continue
		}

		// Rest of the line checking logic...
		for i := 0; i <= segments; i++ {
			t := float64(i) / float64(segments)
			point := r3.Vector{
				X: start.X + (end.X-start.X)*t,
				Y: start.Y + (end.Y-start.Y)*t,
				Z: start.Z + (end.Z-start.Z)*t,
			}

			if point.Sub(smoke.Position).Norm() < smokeRadius {
				if p.debug {
					fmt.Printf("[VISION HACKING] Smoke blocked vision at tick %d (smoke age: %d ticks, EntityId: %d)\n", p.currentTick, ticksSinceDetonate, smoke.EntityId)
				}
				return true
			}
		}
	}
	return false
}

/*func (p *Parser) isLineInSmoke(start, end r3.Vector) bool {
	for _, smokePos := range p.smokePositions {
		// Simple smoke check - if line passes within smoke radius
		smokeRadius := 144.0

		distToLine := distancePointToLine(smokePos.Position, start, end)
		if distToLine < smokeRadius {
			return true
		}
	}
	return false
}*/

func distancePointToLine(point, lineStart, lineEnd r3.Vector) float64 {
	// Calculate distance from point to line segment
	line := lineEnd.Sub(lineStart)
	length := line.Norm()
	if length == 0 {
		return point.Sub(lineStart).Norm()
	}

	t := point.Sub(lineStart).Dot(line) / (length * length)
	t = math.Max(0, math.Min(1, t))

	projection := lineStart.Add(line.Mul(t))
	return point.Sub(projection).Norm()
}

func (p *Parser) SetBSPChecker(bspChecker *BSPVisibilityChecker) {
	if bspChecker == nil {
		fmt.Println("[DEBUG] Warning: BSPChecker is nil.")
	} else {
		fmt.Println("[DEBUG] BSPChecker successfully initialized.")
	}
	p.BspChecker = bspChecker
}

func magnitude(v r3.Vector) float64 {
	return math.Sqrt(v.X*v.X + v.Y*v.Y + v.Z*v.Z)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
