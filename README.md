# High-Concurrency Go HTTP Server (5,000+ Simultaneous Connections)

A lightweight, single-file, production-grade Go HTTP server engineered to handle extreme concurrency locally—sustaining **5,000 simultaneous connections** and delivering **~96,700 requests/sec** with **0 errors**.

---

## Benchmark Highlights

Under a 30-second sustained load test running **5,000 concurrent client connections** hitting a simulated **50ms** backend workload:

* **Throughput:** `96,717.47 requests/sec` (~2.91 million requests in 30 seconds)
* **Average Latency:** `51.34ms` (*only 1.34ms overhead* over the 50ms simulated delay)
* **Socket / Connection Errors:** `0` (100% success rate with HTTP `200 OK`)
* **Efficiency:** Operating at **~99.3%** of theoretical maximum throughput saturation

### Raw `wrk` Benchmark Output

```text
Running 30s test @ http://127.0.0.1:8080/
  12 threads and 5000 connections
  Thread Stats   Avg      Stdev     Max   +/- Stdev
    Latency    51.34ms    2.65ms 118.08ms   93.28%
    Req/Sec     8.12k   407.71     8.76k    89.61%
  2910930 requests in 30.10s, 463.61MB read
Requests/sec:  96717.47
Transfer/sec:     15.40MB
```

<img width="722" height="428" alt="image" src="https://github.com/user-attachments/assets/37183714-9e92-478d-aaa3-55f245f9478c" />



<img width="722" height="428" alt="image" src="https://github.com/user-attachments/assets/4ee004ed-91e5-4038-9f1a-c7d5f8559c36" />


---

## Production Patterns Implemented

1. **Backpressure & Resource Limiting (Buffered Semaphore)**
   * Limits concurrent in-flight request processing to prevent memory exhaustion and goroutine explosion.
   * If the semaphore is saturated, requests queue up to a bounded timeout (`QUEUE_TIMEOUT_MS`).
   * If timeout expires, the server sheds load immediately with `503 Service Unavailable` and a `Retry-After: 1` header.

2. **Connection Lifecycle & Timeout Management**
   * `ReadHeaderTimeout: 2s` — Mitigates Slowloris denial-of-service attacks.
   * `ReadTimeout: 5s` — Prevents slow clients from tying up socket buffers.
   * `WriteTimeout: 10s` — Ensures slow-draining clients do not hold server resources indefinitely.
   * `IdleTimeout: 120s` — Keeps idle keep-alive connections alive for reuse without leaking file descriptors.
   * `MaxHeaderBytes: 1MB` — Prevents memory bloat from oversized headers.

3. **Graceful Shutdown**
   * Captures `SIGINT` (Ctrl+C) and `SIGTERM`.
   * Stops accepting new connections while providing a configurable grace period (`SHUTDOWN_GRACE_SEC`) for active in-flight requests to complete cleanly before terminating.

4. **Context-Aware Request Handling**
   * Simulates a 50ms processing delay using non-blocking timers.
   * Listens to `r.Context().Done()` to immediately abort processing if a client drops or closes the connection prematurely.

5. **Structured JSON Logging**
   * Built entirely using the standard library `log/slog` for structured observability without external dependencies.

---

## Prerequisites & System Tuning

Default Linux configurations limit a single process to **1,024 open file descriptors**, which will fail under 5,000 concurrent connections. Tune the session limits before testing:

### 1. Increase OS File Descriptors and Kernel Limits

```bash
# 1. Raise process file descriptor limit in your terminal session
ulimit -n 65535

# 2. Increase socket listen backlog and enable port reuse (prevents SYN drop during bursts)
sudo sysctl -w net.core.somaxconn=65535
sudo sysctl -w net.ipv4.tcp_max_syn_backlog=65535
sudo sysctl -w net.ipv4.tcp_tw_reuse=1
```

*(Note: `ulimit` resets when you close the terminal window, and `sysctl` resets automatically upon reboot).*

---

## Quickstart

### Step 1: Start the Server

```bash
# In Terminal 1
ulimit -n 65535
go run main.go
```

The server starts on port `8080` with structured logging:
```json
{"time":"...","level":"INFO","msg":"Starting HTTP server","addr":":8080","max_concurrent_workers":5000,"queue_timeout":"200ms","endpoint":"GET / (50ms delay)"}
```

### Step 2: Run the Benchmark

Install `wrk` (recommended) or `hey`:
```bash
sudo apt-get install -y wrk
```

Launch the 5,000 concurrent connection test:
```bash
# In Terminal 2
ulimit -n 65535
wrk -t12 -c5000 -d30s http://127.0.0.1:8080/
```

---

## Configuration (Environment Variables)

All core parameters can be overridden using environment variables:

| Variable | Default | Description |
| :--- | :--- | :--- |
| `PORT` | `8080` | Port the HTTP server binds to. |
| `MAX_CONCURRENT` | `5000` | Maximum number of simultaneous in-flight handlers allowed. |
| `QUEUE_TIMEOUT_MS` | `200ms` | Time a request can wait to acquire a slot before load shedding (HTTP 503). |
| `SHUTDOWN_GRACE_SEC` | `15s` | Maximum time to wait for active requests to finish during graceful shutdown. |

Example:
```bash
MAX_CONCURRENT=2500 QUEUE_TIMEOUT_MS=100ms go run main.go
```

---

## Hardware Tested On

* **Processor:** Intel Core i7-10700K (8 Cores, 16 Threads @ 3.80GHz)
* **Memory:** 32 GB RAM (DDR4)
* **Operating System:** Linux (Kernel 6.x)
* **Runtime:** Go 1.21+
