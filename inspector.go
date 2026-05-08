package client

import "time"

type ConnectionInfo struct {
	ID            int
	Host          string
	Port          int
	Healthy       bool
	ActiveReqs    int32
	HealthScore   int32
	LatencyEWMA   time.Duration
	PipelineDepth int32
	BytesRead     int64
	BytesWritten  int64
}

func (c *Client) ConnectionPool() []ConnectionInfo {
	var out []ConnectionInfo
	c.pool.eachConnection(func(_ string, conn *Connection) {
		out = append(out, ConnectionInfo{
			ID:            conn.id,
			Host:          conn.host,
			Port:          conn.port,
			Healthy:       conn.IsHealthy(),
			ActiveReqs:    conn.activeReqs.Load(),
			HealthScore:   conn.HealthScore(),
			LatencyEWMA:   time.Duration(conn.latencyEWMA.Load()),
			PipelineDepth: conn.pipelineDepth.Load(),
			BytesRead:     conn.bytesRead.Load(),
			BytesWritten:  conn.bytesWritten.Load(),
		})
	})
	return out
}
