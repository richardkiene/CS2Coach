package parser

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/golang/geo/r3"
)

// Model represents a 3D model loaded from an OBJ file
type Model struct {
	triangles []Triangle
	// Bounding information
	min, max r3.Vector
	// Spatial partitioning for map geometry
	sectors  map[string][]Triangle
	gridSize float64
}

// Triangle represents a 3D triangle with three vertices
type Triangle struct {
	v1, v2, v3 r3.Vector
}

// NewModel creates a new model with spatial partitioning initialized
func NewModel() *Model {
	return &Model{
		sectors:  make(map[string][]Triangle),
		gridSize: 256.0, // CS2 commonly uses 256 unit chunks
		min:      r3.Vector{X: 1e10, Y: 1e10, Z: 1e10},
		max:      r3.Vector{X: -1e10, Y: -1e10, Z: -1e10},
	}
}

// LoadOBJ loads a 3D model from an OBJ file and calculates its bounds
func LoadOBJ(filename string) (*Model, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("error opening file: %v", err)
	}
	defer file.Close()

	model := NewModel()
	var vertices []r3.Vector

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "v":
			if len(fields) < 4 {
				continue
			}
			x, _ := strconv.ParseFloat(fields[1], 64)
			y, _ := strconv.ParseFloat(fields[2], 64)
			z, _ := strconv.ParseFloat(fields[3], 64)
			vertex := r3.Vector{X: x, Y: y, Z: z}
			vertices = append(vertices, vertex)

			// Update model bounds
			model.min.X = math.Min(model.min.X, x)
			model.min.Y = math.Min(model.min.Y, y)
			model.min.Z = math.Min(model.min.Z, z)
			model.max.X = math.Max(model.max.X, x)
			model.max.Y = math.Max(model.max.Y, y)
			model.max.Z = math.Max(model.max.Z, z)

		case "f":
			if len(fields) < 4 {
				continue
			}
			v1Idx, _ := strconv.Atoi(strings.Split(fields[1], "/")[0])
			v2Idx, _ := strconv.Atoi(strings.Split(fields[2], "/")[0])
			v3Idx, _ := strconv.Atoi(strings.Split(fields[3], "/")[0])

			triangle := Triangle{
				v1: vertices[v1Idx-1],
				v2: vertices[v2Idx-1],
				v3: vertices[v3Idx-1],
			}
			model.triangles = append(model.triangles, triangle)

			// If this is a map model, add to spatial partitioning
			if model.gridSize > 0 {
				model.addTriangleToSectors(triangle)
			}
		}
	}

	return model, nil
}

// getSectorKey returns a string key for spatial partitioning
func (m *Model) getSectorKey(pos r3.Vector) string {
	x := int(pos.X / m.gridSize)
	y := int(pos.Y / m.gridSize)
	z := int(pos.Z / m.gridSize)
	return fmt.Sprintf("%d:%d:%d", x, y, z)
}

// addTriangleToSectors adds a triangle to relevant spatial sectors
func (m *Model) addTriangleToSectors(t Triangle) {
	// Get affected sectors based on triangle bounds
	minX := math.Min(t.v1.X, math.Min(t.v2.X, t.v3.X))
	maxX := math.Max(t.v1.X, math.Max(t.v2.X, t.v3.X))
	minY := math.Min(t.v1.Y, math.Min(t.v2.Y, t.v3.Y))
	maxY := math.Max(t.v1.Y, math.Max(t.v2.Y, t.v3.Y))
	minZ := math.Min(t.v1.Z, math.Min(t.v2.Z, t.v3.Z))
	maxZ := math.Max(t.v1.Z, math.Max(t.v2.Z, t.v3.Z))

	startX := int(minX / m.gridSize)
	endX := int(maxX / m.gridSize)
	startY := int(minY / m.gridSize)
	endY := int(maxY / m.gridSize)
	startZ := int(minZ / m.gridSize)
	endZ := int(maxZ / m.gridSize)

	for x := startX; x <= endX; x++ {
		for y := startY; y <= endY; y++ {
			for z := startZ; z <= endZ; z++ {
				key := fmt.Sprintf("%d:%d:%d", x, y, z)
				m.sectors[key] = append(m.sectors[key], t)
			}
		}
	}
}

// GetRelevantMapGeometry returns map triangles that could intersect with the line of sight
func (m *Model) GetRelevantMapGeometry(start, end r3.Vector) []Triangle {
	visited := make(map[string]bool)
	var relevantTriangles []Triangle

	// Get all sectors along the line of sight
	dir := r3.Vector{
		X: end.X - start.X,
		Y: end.Y - start.Y,
		Z: end.Z - start.Z,
	}
	length := dir.Norm()
	steps := int(length/m.gridSize) + 1

	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		pos := r3.Vector{
			X: start.X + dir.X*t,
			Y: start.Y + dir.Y*t,
			Z: start.Z + dir.Z*t,
		}

		key := m.getSectorKey(pos)
		if !visited[key] {
			visited[key] = true
			relevantTriangles = append(relevantTriangles, m.sectors[key]...)
		}
	}

	return relevantTriangles
}

// getVisibilityPoints returns key points to check for visibility based on the player model
func getVisibilityPoints(playerModel *Model) []r3.Vector {
	// Get model dimensions
	width := playerModel.max.X - playerModel.min.X
	height := playerModel.max.Z - playerModel.min.Z
	depth := playerModel.max.Y - playerModel.min.Y

	// Create a set of points to check, focusing on vital areas with depth variations
	points := []r3.Vector{
		// Head area (multiple points with depth)
		{X: 0, Y: depth * 0.5, Z: height * 0.9},  // Front of head
		{X: 0, Y: -depth * 0.5, Z: height * 0.9}, // Back of head
		{X: 0, Y: 0, Z: height * 0.8},            // Head center
		// Upper body with depth
		{X: width * 0.3, Y: depth * 0.3, Z: height * 0.7},   // Right shoulder front
		{X: width * 0.3, Y: -depth * 0.3, Z: height * 0.7},  // Right shoulder back
		{X: -width * 0.3, Y: depth * 0.3, Z: height * 0.7},  // Left shoulder front
		{X: -width * 0.3, Y: -depth * 0.3, Z: height * 0.7}, // Left shoulder back
		// Center mass with depth variations
		{X: 0, Y: depth * 0.5, Z: height * 0.5},  // Front center
		{X: 0, Y: -depth * 0.5, Z: height * 0.5}, // Back center
		// Lower body
		{X: 0, Y: depth * 0.3, Z: height * 0.3},  // Lower torso front
		{X: 0, Y: -depth * 0.3, Z: height * 0.3}, // Lower torso back
	}

	return points
}

// CanSeeTarget determines if a player can see a target using view angles and models
func CanSeeTarget(shooter, target PlayerTickData, playerModel, mapModel *Model) bool {
	// Get eye position (using model height)
	eyeHeight := playerModel.max.Z * 0.8 // Approximate eye level
	if shooter.IsCrouched {
		eyeHeight *= 0.75
	}

	eyePos := r3.Vector{
		X: shooter.Position.X,
		Y: shooter.Position.Y,
		Z: shooter.Position.Z + eyeHeight,
	}

	// Get visibility points for target
	points := getVisibilityPoints(playerModel)

	// Transform points to target's position and check each
	for _, basePoint := range points {
		worldPoint := r3.Vector{
			X: basePoint.X + target.Position.X,
			Y: basePoint.Y + target.Position.Y,
			Z: basePoint.Z + target.Position.Z,
		}

		// Check if point is in shooter's FOV first
		if !shooter.IsInFieldOfView(worldPoint) {
			continue
		}

		direction := r3.Vector{
			X: worldPoint.X - eyePos.X,
			Y: worldPoint.Y - eyePos.Y,
			Z: worldPoint.Z - eyePos.Z,
		}.Normalize()

		// Get relevant map geometry and check for intersections
		relevantTriangles := mapModel.GetRelevantMapGeometry(eyePos, worldPoint)
		blocked := false
		for _, triangle := range relevantTriangles {
			if rayIntersectsTriangle(eyePos, direction, triangle) {
				blocked = true
				break
			}
		}

		if !blocked {
			return true
		}
	}

	return false
}

// rayIntersectsTriangle determines if a ray intersects with a triangle
func rayIntersectsTriangle(origin, direction r3.Vector, triangle Triangle) bool {
	const EPSILON = 0.0000001

	edge1 := r3.Vector{
		X: triangle.v2.X - triangle.v1.X,
		Y: triangle.v2.Y - triangle.v1.Y,
		Z: triangle.v2.Z - triangle.v1.Z,
	}
	edge2 := r3.Vector{
		X: triangle.v3.X - triangle.v1.X,
		Y: triangle.v3.Y - triangle.v1.Y,
		Z: triangle.v3.Z - triangle.v1.Z,
	}

	h := r3.Vector{
		X: direction.Y*edge2.Z - direction.Z*edge2.Y,
		Y: direction.Z*edge2.X - direction.X*edge2.Z,
		Z: direction.X*edge2.Y - direction.Y*edge2.X,
	}

	a := edge1.X*h.X + edge1.Y*h.Y + edge1.Z*h.Z
	if a > -EPSILON && a < EPSILON {
		return false
	}

	f := 1.0 / a
	s := r3.Vector{
		X: origin.X - triangle.v1.X,
		Y: origin.Y - triangle.v1.Y,
		Z: origin.Z - triangle.v1.Z,
	}

	u := f * (s.X*h.X + s.Y*h.Y + s.Z*h.Z)
	if u < 0.0 || u > 1.0 {
		return false
	}

	q := r3.Vector{
		X: s.Y*edge1.Z - s.Z*edge1.Y,
		Y: s.Z*edge1.X - s.X*edge1.Z,
		Z: s.X*edge1.Y - s.Y*edge1.X,
	}

	v := f * (direction.X*q.X + direction.Y*q.Y + direction.Z*q.Z)
	if v < 0.0 || u+v > 1.0 {
		return false
	}

	t := f * (edge2.X*q.X + edge2.Y*q.Y + edge2.Z*q.Z)
	return t > EPSILON
}
