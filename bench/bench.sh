#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
PROXY_HOST="${PROXY_HOST:-http://localhost:4001}"
RESULTS_DIR="$SCRIPT_DIR/results"
KEYS_FILE="$PROJECT_DIR/keys.json"
VENV_DIR="$SCRIPT_DIR/.venv"

# Extract first proxy key from keys.json
AUTH_KEY=$(python3 -c "
import json
ks = json.load(open('$KEYS_FILE'))
print(next(iter(ks['users'].values()))['key'])
")

# Check dependencies
if ! command -v hey &>/dev/null; then
    echo "ERROR: hey not found. Install with: GOBIN=~/.local/bin go install github.com/rakyll/hey@latest"
    exit 1
fi

# Verify proxy is reachable
if ! curl -sf "$PROXY_HOST/health" >/dev/null 2>&1; then
    echo "ERROR: Proxy not responding at $PROXY_HOST/health"
    echo "Start it with: $PROJECT_DIR/simple-api-proxy serve -port 4001"
    exit 1
fi

mkdir -p "$RESULTS_DIR"

run_test() {
    local name="$1"
    local conc="$2"
    local n="$3"
    local outfile="$RESULTS_DIR/${name}_c${conc}.csv"
    shift 3
    # remaining args are extra hey flags

    echo "  ${name} | concurrency=${conc} | n=${n}"
    hey -n "$n" -c "$conc" -o csv "$@" > "$outfile" 2>/dev/null
    local lines=$(( $(wc -l < "$outfile") - 1 ))
    echo "    -> $lines requests recorded"
}

echo "========================================"
echo "TIER 1: /health (no auth, no upstream)"
echo "========================================"
for c in 10 25 50 100 200; do
    run_test "health" "$c" 1000 "$PROXY_HOST/health"
done

echo ""
echo "========================================"
echo "TIER 2: /v1/models (auth + upstream)"
echo "========================================"
for c in 5 10 20 50; do
    run_test "models" "$c" 200 \
        -H "Authorization: Bearer $AUTH_KEY" \
        "$PROXY_HOST/v1/models"
done

echo ""
echo "========================================"
echo "TIER 3: /v1/chat/completions (streaming)"
echo "========================================"
CHAT_BODY='{"model":"gpt-4o","messages":[{"role":"user","content":"Say hi"}],"max_tokens":5,"stream":true}'
for c in 2 5 10; do
    run_test "chat" "$c" 20 \
        -m POST \
        -H "Authorization: Bearer $AUTH_KEY" \
        -H "Content-Type: application/json" \
        -d "$CHAT_BODY" \
        -t 120 \
        "$PROXY_HOST/v1/chat/completions"
done

# Aggregate results into summary.json
echo ""
echo "Aggregating results..."
python3 << 'PYEOF'
import csv, json, os, statistics

results_dir = os.environ.get("RESULTS_DIR", "results")
summary = {}

for fname in sorted(os.listdir(results_dir)):
    if not fname.endswith(".csv"):
        continue

    name = fname.replace(".csv", "")
    parts = name.rsplit("_c", 1)
    tier = parts[0]
    conc = int(parts[1])

    times = []
    errors = 0
    total = 0
    offsets = []

    with open(os.path.join(results_dir, fname)) as f:
        reader = csv.DictReader(f)
        for row in reader:
            total += 1
            rt = float(row["response-time"])
            code = int(row["status-code"])
            times.append(rt)
            offsets.append(float(row["offset"]))
            if code >= 400 or code == 0:
                errors += 1

    if not times:
        continue

    times.sort()
    n = len(times)
    wall_time = max(offsets) - min(offsets) if len(offsets) > 1 else 0
    rps = n / wall_time if wall_time > 0 else 0

    def percentile(data, p):
        k = (len(data) - 1) * (p / 100)
        f = int(k)
        c = f + 1 if f + 1 < len(data) else f
        d = k - f
        return data[f] + d * (data[c] - data[f])

    summary.setdefault(tier, []).append({
        "concurrency": conc,
        "total": total,
        "errors": errors,
        "error_rate": errors / total if total else 0,
        "rps": round(rps, 2),
        "p50": round(percentile(times, 50) * 1000, 2),
        "p95": round(percentile(times, 95) * 1000, 2),
        "p99": round(percentile(times, 99) * 1000, 2),
        "min": round(min(times) * 1000, 2),
        "max": round(max(times) * 1000, 2),
        "mean": round(statistics.mean(times) * 1000, 2),
    })

for tier in summary:
    summary[tier].sort(key=lambda x: x["concurrency"])

with open(os.path.join(results_dir, "summary.json"), "w") as f:
    json.dump(summary, f, indent=2)

# Print table
for tier, entries in sorted(summary.items()):
    print(f"\n--- {tier} ---")
    print(f"{'conc':>5} {'rps':>8} {'p50ms':>8} {'p95ms':>8} {'p99ms':>8} {'err%':>6}")
    for e in entries:
        print(f"{e['concurrency']:5d} {e['rps']:8.1f} {e['p50']:8.2f} {e['p95']:8.2f} {e['p99']:8.2f} {e['error_rate']*100:5.1f}%")
PYEOF

echo ""
echo "Summary written to $RESULTS_DIR/summary.json"

# Generate plots
echo ""
echo "Generating plots..."
"$VENV_DIR/bin/python3" "$SCRIPT_DIR/plot_results.py"

echo ""
echo "Done. Results in $RESULTS_DIR/, plots in $SCRIPT_DIR/plots/"
