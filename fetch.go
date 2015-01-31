package main

import (
	"fmt"
	r "github.com/scascketta/capmetro-data/Godeps/_workspace/src/github.com/dancannon/gorethink"
	"time"
)

var (
	lastUpdated map[string]time.Time = map[string]time.Time{}

	firstNewVehicleCheck bool          = true
	nextNewVehicleCheck  time.Time     = time.Now()
	vehicleCheckInterval time.Duration = (4 * 60 * 60) * (1000 * time.Millisecond)

	normalDuration   time.Duration = (30) * (1000 * time.Millisecond)
	extendedDuration time.Duration = (10 * 60) * (1000 * time.Millisecond)

	emptyResponses      map[string]int  = map[string]int{}
	recentEmptyResponse map[string]bool = map[string]bool{}
)

type VehicleStopTime struct {
	VehicleID string    `gorethink:"vehicle_id"`
	Route     string    `gorethink:"route"`
	TripID    string    `gorethink:"trip_id"`
	StopID    string    `gorethink:"stop_id"`
	Time      time.Time `gorethink:"timestamp"`
}

func FilterUpdatedVehicles(vehicles []VehiclePosition) []VehiclePosition {
	updated := []VehiclePosition{}
	for _, v := range vehicles {
		updateTime, _ := lastUpdated[v.VehicleID]
		lastUpdated[v.VehicleID] = v.Time
		if !updateTime.Equal(v.Time) {
			updated = append(updated, v)
		}
	}
	return updated
}

func LogVehiclePositions(session *r.Session, route string) error {
	vehicles, err := FetchVehicles(route)
	if err != nil {
		return err
	}
	if vehicles == nil {
		// increment retry count if fetch just before was also empty
		// only subsequent empty responses matter when determining how long to sleep
		if recentEmptyResponse[route] {
			emptyResponses[route] += 1
		}
		recentEmptyResponse[route] = true
		return fmt.Errorf("No vehicles in response for route: %s.", route)
	} else {
		recentEmptyResponse[route] = false
	}

	updated := FilterUpdatedVehicles(vehicles)

	for _, v := range updated {
		dbglogger.Printf("Vehicle %s updated at %s\n", v.VehicleID, v.Time.Format("2006-01-02T15:04:05-07:00"))
	}

	if len(updated) > 0 {
		_, err = r.Table("vehicle_position").Insert(r.Expr(updated)).Run(session)
		if err != nil {
			return err
		}
		dbglogger.Printf("Log %d vehicles, route %s.\n", len(updated), route)
	} else {
		dbglogger.Printf("No new vehicle positions to record for route %s.\n", route)
	}
	return nil
}

// Check if the the routes are inactive
// There must have been MAX_RETRIES previous attempts to fetch data,
// and all attempts must have failed
func routesAreSleeping() bool {
	dbglogger.Println("emptyResponses:", emptyResponses)
	dbglogger.Println("recentEmptyResponse:", recentEmptyResponse)
	for _, retries := range emptyResponses {
		if retries < MAX_RETRIES {
			return false
		}
	}
	return true
}

// Check if any new vehicles appear in recorded vehicle positions, add them to vehicles table
func checkNewVehicles(session *r.Session) error {
	new_vehicles := 0
	dbglogger.Println("Check for new vehicles.")
	vehicles := []map[string]string{}
	cur, err := r.Table("vehicle_position").Pluck("vehicle_id", "route", "route_id", "trip_id").Distinct().Run(session)
	if err != nil {
		return err
	}
	cur.All(&vehicles)

	for _, data := range vehicles {
		id := data["vehicle_id"]
		stream := r.Table("vehicles").Pluck("vehicle_id")
		query_expr := r.Expr(map[string]string{"vehicle_id": data["vehicle_id"]})
		cur, err = stream.Contains(query_expr).Run(session)
		if err != nil {
			return err
		}
		var res bool

		cur.Next(&res)
		if !res {
			new_vehicles += 1
			dbglogger.Printf("Adding new vehicle %s to vehicles table.\n", id)
			vehicle := Vehicle{
				VehicleID:    data["vehicle_id"],
				Route:        data["route"],
				RouteID:      data["route_id"],
				TripID:       data["trip_id"],
				LastAnalyzed: time.Now(),
			}
			_, err := r.Table("vehicles").Insert(r.Expr(vehicle)).Run(session)
			if err != nil {
				return err
			}
		}
	}

	dbglogger.Printf("Inserted %d new vehicles.\n", new_vehicles)
	return nil
}

// Find closest stop at a given time for each recorded vehicle position
func MakeVehicleStopTimes(session *r.Session) error {
	vehicles := []Vehicle{}
	cur, err := r.Db("capmetro").Table("vehicles").Run(session)
	if err != nil {
		return err
	}
	cur.All(&vehicles)
	if vehicles == nil {
		return fmt.Errorf("No vehicles available for making stop times.")
	}

	for _, vehicle := range vehicles {
		stop_times := []VehicleStopTime{}
		// Not using []VehiclePosition because gorethink has trouble unmarshaling the Location field
		positions := []map[string]interface{}{}

		// get all vehicle_positions for vehicle_id after vehicle.LastAnalyzed
		// vehicle_id_timestamp is a compound index on a vehicle's id and timestamp
		between_opts := r.BetweenOpts{Index: "vehicle_id_timestamp"}
		lower_key := r.Expr([]interface{}{vehicle.VehicleID, vehicle.LastAnalyzed})
		upper_key := r.Expr([]interface{}{vehicle.VehicleID, r.EpochTime(2000005200)})
		query := r.Db("capmetro").Table("vehicle_position")
		query = query.Between(lower_key, upper_key, between_opts)
		cur, err := query.Run(session)
		if err != nil {
			errlogger.Println(err)
			continue
		}
		cur.All(&positions)
		if positions == nil {
			dbglogger.Printf("No positions available for vehicle %s after %s.\n", vehicle.VehicleID, vehicle.LastAnalyzed.Format("2006-01-02T15:04:05-07:00"))
			continue
		}

		dbglogger.Printf("Processing %d positions for vehicle %s after %s.\n", len(positions), vehicle.VehicleID, vehicle.LastAnalyzed.Format("2006-01-02T15:04:05-07:00"))
		for _, position := range positions {
			stops := []map[string]interface{}{}
			gn_opts := r.GetNearestOpts{Index: "location", MaxDist: 100, MaxResults: 1}
			query := r.Db("capmetro").Table("stops").GetNearest(position["location"], gn_opts)
			cur, err := query.Run(session)
			if err != nil {
				errlogger.Println(err)
				continue
			}
			cur.All(&stops)
			if len(stops) == 0 {
				continue
			}
			stop := stops[0]["doc"].(map[string]interface{})
			stop_time := VehicleStopTime{
				VehicleID: vehicle.VehicleID,
				Route:     position["route"].(string),
				TripID:    position["trip_id"].(string),
				StopID:    stop["stop_id"].(string),
				Time:      position["timestamp"].(time.Time),
			}
			if len(stop_times) > 0 {
				// Don't want to log stop_time at the same stop with later timestamp
				recent_stop := stop_times[len(stop_times)-1]
				recent_stopid_matches := recent_stop.StopID == stop_time.StopID
				recent_stop_before := recent_stop.Time.Before(stop_time.Time)
				if recent_stopid_matches && recent_stop_before {
					dbglogger.Println("Skip stoptime")
					continue
				}
			}
			stop_times = append(stop_times, stop_time)
			dbglogger.Printf("Added stop_time: stop=%s, time=%s.\n", stop_time.StopID, stop_time.Time.Format("2006-01-02T15:04:05-07:00"))
		}
		_, err = r.Db("capmetro").Table("vehicle_stop_times").Insert(r.Expr(stop_times)).Run(session)
		if err != nil {
			errlogger.Println(err)
		}
		dbglogger.Printf("Added %d stop times for vehicle %s.\n", len(stop_times), vehicle.VehicleID)
		vehicle.LastAnalyzed = time.Now()
		_, err = r.Db("capmetro").Table("vehicles").Get(vehicle.ID).Update(r.Expr(vehicle)).RunWrite(session)
		if err != nil {
			return err
		}
	}
	return nil
}
