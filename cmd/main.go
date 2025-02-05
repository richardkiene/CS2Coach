package main

import (
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/richardkiene/CS2Coach/internal/analyzer"
	"github.com/richardkiene/CS2Coach/internal/ml"
	"github.com/richardkiene/CS2Coach/internal/models"
	"github.com/richardkiene/CS2Coach/internal/parser"
)

func main() {
	analyzeCmd := flag.NewFlagSet("analyze", flag.ExitOnError)
	trainCmd := flag.NewFlagSet("train", flag.ExitOnError)
	predictCmd := flag.NewFlagSet("predict", flag.ExitOnError)
	inspectVPKCmd := flag.NewFlagSet("inspect-vpk", flag.ExitOnError)

	// Analyze command flags
	analyzeDemoPath := analyzeCmd.String("demo", "", "Path to CS2 demo file")
	analyzePlayerName := analyzeCmd.String("player", "", "Player name to analyze")
	analyzeSteamID := analyzeCmd.String("steamid", "", "Steam ID to analyze")
	analyzeDebug := analyzeCmd.Bool("debug", false, "Enable debug output")
	analyzeVerbose := analyzeCmd.Bool("verbose", false, "Enable verbose output")

	// Train command flags
	trainDemoDir := trainCmd.String("demodir", "", "Directory containing demo files for training")
	trainConfigPath := trainCmd.String("config", "", "Path to model configuration file")

	// Predict command flags
	predictDemoPath := predictCmd.String("demo", "", "Path to CS2 demo file")
	predictPlayerName := predictCmd.String("player", "", "Player name to predict")
	predictSteamID := predictCmd.String("steamid", "", "Steam ID to predict")

	// Inspect VPK command flags
	path := inspectVPKCmd.String("path", "", "Path to VPK file or directory")
	outputDir := inspectVPKCmd.String("output", "", "Output directory for extracted files")
	mapName := inspectVPKCmd.String("map", "", "Map name to extract (without .bsp extension)")
	recursive := inspectVPKCmd.Bool("recursive", false, "Recursively process subdirectories")

	if len(os.Args) < 2 {
		fmt.Println("Expected 'analyze', 'train', 'predict', or 'inspect-vpk' subcommands")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "analyze":
		analyzeCmd.Parse(os.Args[2:])
		handleAnalyze(*analyzeDemoPath, *analyzePlayerName, *analyzeSteamID, *analyzeDebug, *analyzeVerbose)
	case "train":
		trainCmd.Parse(os.Args[2:])
		handleTrain(*trainDemoDir, *trainConfigPath, *analyzeDebug, *analyzeVerbose)
	case "predict":
		predictCmd.Parse(os.Args[2:])
		handlePredict(*predictDemoPath, *predictPlayerName, *predictSteamID, *analyzeDebug, *analyzeVerbose)
	case "inspect-vpk":
		inspectVPKCmd.Parse(os.Args[2:])
		handleInspectVPK(*path, *outputDir, *mapName, *recursive)
	default:
		fmt.Printf("%q is not valid command.\n", os.Args[1])
		os.Exit(1)
	}
}

func handleInspectVPK(path, outputDir, mapName string, recursive bool) {
	if path == "" {
		log.Fatal("Please provide a VPK file path or directory")
	}

	if outputDir == "" {
		outputDir = filepath.Join(os.TempDir(), "cs2coach_bsp_test")
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	logLevel := slog.LevelDebug
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))

	loader := parser.NewBSPLoader(path, *logger)

	_, err := loader.LoadBSPForMap(mapName)
	if err != nil {
		log.Fatalf("Failed to find BSP for map %s: %v", mapName, err)
	}

	fmt.Printf("Successfully loaded BSP for map %s\n", mapName)
}

func handleAnalyze(demoPath, playerName, steamID string, debug, verbose bool) {
	if demoPath == "" {
		log.Fatal("Please provide a demo file path")
	}
	if playerName == "" && steamID == "" {
		log.Fatal("Please provide either player name or Steam ID")
	}

	var logLevel slog.Level
	if debug {
		logLevel = slog.LevelDebug
	} else if verbose {
		logLevel = slog.LevelWarn
	} else {
		logLevel = slog.LevelInfo
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))

	c := parser.NewCollector(logger)
	match, err := c.Collect(demoPath)
	c.AnalyzeTimeToDamage()
	logger.Error("Error collecting demo data from demoPath: %s err: %s\n", demoPath, err)

	mapName := match.MapName
	if mapName == "" {
		log.Fatal("Unable to determine map name from demo file")
	}
	logger.Info("Map detected", "mapName", mapName)

	// Step 2: Analyze the parsed match data
	a := analyzer.NewAnalyzer()
	stats := a.AnalyzeMatch(match, playerName, steamID, verbose)
	if stats == nil {
		log.Fatal("Player not found in demo")
	}

	// Step 3: Display the results
	displayLeetifyMetrics(stats, match, playerName, verbose)
}

func displayLeetifyMetrics(stats *models.AnalyzedStats, match *models.Match, playerName string, verbose bool) {
	// Calculate metrics
	roundCount := stats.BasicStats.RoundsActive
	fmt.Println("stats.BasicStats.RoundsActive: " + fmt.Sprint(stats.BasicStats.RoundsActive))
	leetifyMetrics := analyzer.CalculateLeetifyMetrics(&stats.BasicStats, roundCount)

	fmt.Printf("\nLeetify Analysis for: %s\n", playerName)
	fmt.Printf("\nMap: %s\n", match.MapName)

	// Overview
	fmt.Printf("\nOverview Metrics:\n")
	fmt.Printf("Leetify Rating: %.2f\n", leetifyMetrics.LeetifyRating)
	fmt.Printf("HLTV Rating: %.2f\n", leetifyMetrics.HLTV)
	fmt.Printf("ADR: %.1f\n", leetifyMetrics.ADR)

	// Combat Metrics
	fmt.Printf("\nCombat Performance:\n")
	fmt.Printf("- K/D/A: %d/%d/%d\n", stats.BasicStats.Kills, stats.BasicStats.Deaths, stats.BasicStats.Assists)
	fmt.Printf("- Accuracy (All): %.1f%%\n", leetifyMetrics.AccuracyAll)
	fmt.Printf("- Spotted Accuracy: %.1f%%\n", leetifyMetrics.SpottedAccuracy)
	fmt.Printf("- Head Accuracy: %.1f%%\n", leetifyMetrics.HeadAccuracy)
	fmt.Printf("- Headshot Kill %%: %.1f%%\n", leetifyMetrics.HeadshotKillPercentage)
	fmt.Printf("- Spray Accuracy: %.1f%%\n", leetifyMetrics.SprayAccuracy)
	fmt.Printf("- Counter-Strafing: %.1f%%\n", leetifyMetrics.CounterStrafing)
	fmt.Printf("- Time to Damage: %.0fms\n", leetifyMetrics.TimeToFirstDamage)
	fmt.Printf("- Crosshair Placement: %.2f°\n", leetifyMetrics.CrosshairPlacement)

	// Trade Metrics
	fmt.Printf("\nTrade Statistics:\n")
	fmt.Printf("- Trade Kill Attempts: %.1f%% (%d/%d)\n",
		leetifyMetrics.TradeKillAttemptRate,
		stats.BasicStats.TradeKillAttempts,
		stats.BasicStats.TradeKillOpportunities)
	fmt.Printf("- Trade Kill Success: %.1f%% (%d/%d)\n",
		leetifyMetrics.TradeKillSuccessRate,
		stats.BasicStats.TradeKills,
		stats.BasicStats.TradeKillAttempts)
	fmt.Printf("- Traded Death Attempts: %.1f%% (%d/%d)\n",
		leetifyMetrics.TradedDeathAttemptRate,
		stats.BasicStats.TradedDeathAttempts,
		stats.BasicStats.TradedDeathOpportunities)
	fmt.Printf("- Traded Death Success: %.1f%% (%d/%d)\n",
		leetifyMetrics.TradedDeathSuccessRate,
		stats.BasicStats.TradedDeaths,
		stats.BasicStats.TradedDeathAttempts)

	// Utility Metrics
	fmt.Printf("\nUtility Impact:\n")
	fmt.Printf("- Flash Assists: %.1f per game\n", leetifyMetrics.UtilityMetrics.FlashAssistsPerGame)
	fmt.Printf("- Enemies Flashed: %.2f per game\n", leetifyMetrics.UtilityMetrics.EnemiesFlashedPerGame)
	fmt.Printf("- Friends Flashed: %.2f per game\n", leetifyMetrics.UtilityMetrics.TeammatesFlashedPerGame)
	fmt.Printf("- Avg Blind Duration: %.1fs\n", leetifyMetrics.UtilityMetrics.AvgBlindDuration)
	fmt.Printf("- Avg HE Damage: %.2f\n", leetifyMetrics.AvgHEDamage)
	fmt.Printf("- Avg HE Team Damage: %.2f\n", leetifyMetrics.AvgTeamHEDamage)
	fmt.Printf("- Avg Unused Utility: $%.0f\n", leetifyMetrics.AvgUnusedUtilityValue)

	if verbose {
		// Multi-kill stats
		fmt.Printf("\nMulti-kill Rounds:\n")
		fmt.Printf("- Two Kills: %d\n", leetifyMetrics.MultiKills["two"])
		fmt.Printf("- Three Kills: %d\n", leetifyMetrics.MultiKills["three"])
		fmt.Printf("- Four Kills: %d\n", leetifyMetrics.MultiKills["four"])
		fmt.Printf("- Five Kills: %d\n", leetifyMetrics.MultiKills["five"])

		// Detailed weapon stats
		fmt.Printf("\nDetailed Weapon Statistics:\n")
		for weapon, stats := range stats.BasicStats.WeaponStats {
			if stats.Shots > 0 {
				accuracy := float64(stats.Hits) / float64(stats.Shots) * 100
				hsRate := float64(stats.Headshots) / float64(stats.Kills) * 100
				fmt.Printf("- %s: %d kills, %.1f%% accuracy, %.1f%% HS\n",
					weapon, stats.Kills, accuracy, hsRate)
			}
		}
	}
}

func handleTrain(demoDir, configPath string, debug bool, verbose bool) {
	if demoDir == "" {
		log.Fatal("Please provide a demo directory path")
	}

	// Initialize model
	model, err := ml.NewModel(configPath)
	if err != nil {
		log.Fatalf("Error creating model: %v", err)
	}

	// Collect demo files
	var matches []*models.Match
	err = filepath.Walk(demoDir, func(path string, info os.FileInfo, err error) error {
		if filepath.Ext(path) == ".dem" {
			p := parser.NewParser(debug)
			match, err := p.ParseDemo(path, debug)
			if err != nil {
				fmt.Printf("Warning: Error parsing demo %s: %v\n", path, err)
				return nil
			}
			matches = append(matches, match)
		}
		return nil
	})

	if err != nil {
		log.Fatalf("Error walking demo directory: %v", err)
	}

	fmt.Printf("Training model on %d demos...\n", len(matches))
	if err := model.Train(matches); err != nil {
		log.Fatalf("Error training model: %v", err)
	}

	fmt.Println("Model training completed successfully")
}

func handlePredict(demoPath, playerName, steamID string, debug bool, verbose bool) {
	if demoPath == "" {
		log.Fatal("Please provide a demo file path")
	}
	if playerName == "" && steamID == "" {
		log.Fatal("Please provide either player name or Steam ID")
	}

	// Parse demo
	p := parser.NewParser(debug)
	match, err := p.ParseDemo(demoPath, debug)
	if err != nil {
		log.Fatalf("Error parsing demo: %v", err)
	}

	// Analyze match
	a := analyzer.NewAnalyzer()
	stats := a.AnalyzeMatch(match, playerName, steamID, verbose)
	if stats == nil {
		log.Fatal("Player not found in demo")
	}

	// Initialize model and predict
	model, err := ml.NewModel("")
	if err != nil {
		log.Fatalf("Error creating model: %v", err)
	}

	prediction, err := model.Predict(stats)
	if err != nil {
		log.Fatalf("Error making prediction: %v", err)
	}

	// Display metrics with map info
	displayLeetifyMetrics(stats, match, playerName, verbose)

	fmt.Printf("\nPrediction Results:\n")
	fmt.Printf("Player Impact: %s (%.2f%% confidence)\n",
		prediction.Outcome, prediction.Probability*100)
}
