package database

import (
	"math"
	"slices"
	"time"

	"github.com/codingsince1985/geo-golang"
	"github.com/codingsince1985/geo-golang/openstreetmap"
	"github.com/tkrajina/gpxgo/gpx"
	"github.com/westphae/geomag/pkg/egm96"
	"gorm.io/gorm"
)

var online = true

var correctAltitudeCreators = []string{
	"Garmin", "Garmin Connect",
	"Apple Watch",
	"StravaGPX iPhone", "StravaGPX",
}

func creatorNeedsCorrection(creator string) bool {
	return !slices.Contains(correctAltitudeCreators, creator)
}

func normalizeDegrees(val float64) float64 {
	if val < 0 {
		return val + 360
	}

	return val
}

func correctAltitude(creator string, lat, long, alt float64) float64 {
	if !creatorNeedsCorrection(creator) {
		return alt
	}

	lat = normalizeDegrees(lat)
	long = normalizeDegrees(long)

	loc := egm96.NewLocationGeodetic(lat, long, alt)

	h, err := loc.HeightAboveMSL()
	if err != nil {
		return alt
	}

	return h
}

type MapData struct {
	gorm.Model
	WorkoutID     uint            `gorm:"not null;uniqueIndex"` // The workout this data belongs to
	Creator       string          // The tool that created this workout
	Name          string          // The name of the workout
	Center        MapCenter       `gorm:"serializer:json"` // The center of the workout (in coordinates)
	Address       *geo.Address    `gorm:"serializer:json"` // The address of the workout
	TotalDistance float64         // The total distance of the workout
	TotalDuration time.Duration   // The total duration of the workout
	MaxSpeed      float64         // The maximum speed of the workout
	PauseDuration time.Duration   // The total pause duration of the workout
	MinElevation  float64         // The minimum elevation of the workout
	MaxElevation  float64         // The maximum elevation of the workout
	TotalUp       float64         // The total distance up of the workout
	TotalDown     float64         // The total distance down of the workout
	Details       *MapDataDetails `json:",omitempty"` // The details of the workout

	Points []MapPoint `gorm:"serializer:json" json:"-"` // To be removed
}

type MapDataDetails struct {
	gorm.Model
	MapDataID uint       // The ID of the map data these details belong to
	Points    []MapPoint `gorm:"serializer:json"` // The GPS points of the workout
}

type MapCenter struct {
	Lat float64 // The latitude of the center of the workout
	Lng float64 // The longitude of the center of the workout
}
type MapPoint struct {
	Lat           float64       // The latitude of the point
	Lng           float64       // The longitude of the point
	Distance      float64       // The distance from the previous point
	TotalDistance float64       // The total distance of the workout up to this point
	Duration      time.Duration // The duration from the previous point
	TotalDuration time.Duration // The total duration of the workout up to this point
	Time          time.Time     // The time the point was recorded
	Elevation     float64       // The elevation of the point
}

func (d *MapDataDetails) Save(db *gorm.DB) error {
	return db.Save(d).Error
}

func (m *MapData) Save(db *gorm.DB) error {
	return db.Save(m).Error
}

func (m *MapData) AverageSpeed() float64 {
	return m.TotalDistance / m.TotalDuration.Seconds()
}

func (m *MapData) AverageSpeedNoPause() float64 {
	return m.TotalDistance / (m.TotalDuration - m.PauseDuration).Seconds()
}

func (m *MapPoint) AverageSpeed() float64 {
	return m.Distance / m.Duration.Seconds()
}

// center returns the center point (lat, lng) of gpx points
func center(gpxContent *gpx.GPX) MapCenter {
	points := allGPXPoints(gpxContent)

	if len(points) == 0 {
		return MapCenter{}
	}

	lat, lng := 0.0, 0.0

	for _, pt := range points {
		lat += pt.Point.Latitude
		lng += pt.Point.Longitude
	}

	size := float64(len(points))

	return MapCenter{
		Lat: lat / size,
		Lng: lng / size,
	}
}

func (m *MapCenter) Address() *geo.Address {
	if !online {
		return nil
	}

	geocoder := openstreetmap.Geocoder()

	address, err := geocoder.ReverseGeocode(m.Lat, m.Lng)
	if err != nil {
		return nil
	}

	return address
}

// allGPXPoints returns the first track segment's points
func allGPXPoints(gpxContent *gpx.GPX) []gpx.GPXPoint {
	var points []gpx.GPXPoint

	for _, track := range gpxContent.Tracks {
		for _, segment := range track.Segments {
			for _, p := range segment.Points {
				if !pointHasDistance(p) {
					continue
				}

				points = append(points, p)
			}
		}
	}

	return points
}

func pointHasDistance(p gpx.GPXPoint) bool {
	if math.IsNaN(p.Latitude) || math.IsNaN(p.Longitude) {
		return false
	}

	return true
}

func gpxName(gpxContent *gpx.GPX) string {
	if gpxContent.Name != "" {
		return gpxContent.Name
	}

	if len(gpxContent.Tracks) == 0 {
		return "(no name)"
	}

	return gpxContent.Tracks[0].Name
}

// Determines the date to use for the workout
func gpxDate(gpxContent *gpx.GPX) *time.Time {
	// Use the first track's first segment's timestamp if it exists
	// This is the best time to use as a start time, since converters shouldn't
	// touch this timestamp
	if len(gpxContent.Tracks) > 0 {
		if t := gpxContent.Tracks[0]; len(t.Segments) > 0 {
			if s := t.Segments[0]; len(s.Points) > 0 {
				return &s.Points[0].Timestamp
			}
		}
	}

	// Otherwise, return the timestamp from the metadata, use that (not all apps have
	// this, notably Workoutdoors doesn't)
	// If this is nil, this should result in an error and the user will be alerted.
	return gpxContent.Time
}

func distanceBetween(p1 gpx.GPXPoint, p2 gpx.GPXPoint) float64 {
	return gpx.HaversineDistance(p1.Latitude, p1.Longitude, p2.Latitude, p2.Longitude)
}

func createMapData(gpxContent *gpx.GPX) *MapData {
	if len(gpxContent.Tracks) == 0 {
		return nil
	}

	var (
		totalDistance, maxElevation, uphill, downhill, maxSpeed float64
		totalDuration, pauseDuration                            time.Duration
	)

	minElevation := 100000.0 // This should be high enough for Earthly workouts

	for _, track := range gpxContent.Tracks {
		for _, segment := range track.Segments {
			if len(segment.Points) == 0 {
				continue
			}

			totalDistance += segment.Length3D()
			totalDuration += time.Duration(segment.Duration()) * time.Second
			pauseDuration += (time.Duration(segment.MovingData().StoppedTime)) * time.Second
			minElevation = min(minElevation, segment.ElevationBounds().MinElevation)
			maxElevation = max(maxElevation, segment.ElevationBounds().MaxElevation)
			uphill += segment.UphillDownhill().Uphill
			downhill += segment.UphillDownhill().Downhill
			maxSpeed = max(maxSpeed, segment.MovingData().MaxSpeed)
			pauseDuration += time.Duration(segment.MovingData().StoppedTime)
		}
	}

	// Make sure minElevation is never higher than maxElevation
	minElevation = min(minElevation, maxElevation)

	// Now reduce the whole GPX to a single track to calculate the center
	gpxContent.ReduceGpxToSingleTrack()
	mapCenter := center(gpxContent)

	data := &MapData{
		Creator:       gpxContent.Creator,
		Name:          gpxName(gpxContent),
		Center:        mapCenter,
		Address:       mapCenter.Address(),
		TotalDistance: totalDistance,
		TotalDuration: totalDuration,
		MaxSpeed:      maxSpeed,
		PauseDuration: pauseDuration,
		MinElevation:  correctAltitude(gpxContent.Creator, mapCenter.Lat, mapCenter.Lng, minElevation),
		MaxElevation:  correctAltitude(gpxContent.Creator, mapCenter.Lat, mapCenter.Lng, maxElevation),
		TotalUp:       uphill,
		TotalDown:     downhill,
	}

	data.correctNaN()

	return data
}

func (m *MapData) correctNaN() {
	if math.IsNaN(m.MinElevation) {
		m.MinElevation = 0
	}

	if math.IsNaN(m.MaxElevation) {
		m.MaxElevation = 0
	}

	if math.IsNaN(m.TotalDistance) {
		m.TotalDistance = 0
	}

	if math.IsNaN(m.TotalDown) {
		m.TotalDown = 0
	}

	if math.IsNaN(m.TotalUp) {
		m.TotalUp = 0
	}
}

func gpxAsMapData(gpxContent *gpx.GPX) *MapData {
	data := createMapData(gpxContent)

	points := allGPXPoints(gpxContent)
	if len(points) == 0 {
		return data
	}

	totalDist := 0.0
	totalTime := 0.0
	prevPoint := points[0]

	data.Details = &MapDataDetails{}

	for i, pt := range points {
		if !pointHasDistance(pt) {
			continue
		}

		dist := 0.0
		t := 0.0

		if i > 0 {
			dist = distanceBetween(prevPoint, pt)
			t = pt.TimeDiff(&prevPoint)

			prevPoint = pt
		}

		totalDist += dist
		totalTime += t

		data.Details.Points = append(data.Details.Points, MapPoint{
			Lat:           pt.Point.Latitude,
			Lng:           pt.Point.Longitude,
			Time:          pt.Timestamp,
			Distance:      dist,
			TotalDistance: totalDist,
			Duration:      time.Duration(t) * time.Second,
			TotalDuration: time.Duration(totalTime) * time.Second,
			Elevation:     correctAltitude(gpxContent.Creator, pt.Point.Latitude, pt.Point.Longitude, pt.Elevation.Value()),
		})
	}

	return data
}