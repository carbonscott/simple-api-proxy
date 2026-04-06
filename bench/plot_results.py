#!/usr/bin/env python3
"""Read results/summary.json and generate benchmark plots."""

import json
import os

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import matplotlib.ticker as ticker

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
RESULTS_DIR = os.path.join(SCRIPT_DIR, "results")
PLOTS_DIR = os.path.join(SCRIPT_DIR, "plots")

TIER_COLORS = {"health": "#4CAF50", "models": "#2196F3", "chat": "#9C27B0"}
TIER_MARKERS = {"health": "o", "models": "s", "chat": "^"}
PCT_COLORS = {"p50": "#2196F3", "p95": "#FF9800", "p99": "#F44336"}


def load_summary():
    with open(os.path.join(RESULTS_DIR, "summary.json")) as f:
        return json.load(f)


def setup_style():
    plt.rcParams.update({
        "figure.figsize": (10, 6),
        "font.size": 12,
        "axes.grid": True,
        "grid.alpha": 0.3,
        "lines.linewidth": 2,
        "lines.markersize": 8,
    })


def plot_latency(summary):
    """One subplot per tier showing p50/p95/p99 vs concurrency."""
    tiers = sorted(summary.keys())
    fig, axes = plt.subplots(1, len(tiers), figsize=(6 * len(tiers), 5), squeeze=False)

    for idx, tier in enumerate(tiers):
        ax = axes[0][idx]
        entries = summary[tier]
        concs = [e["concurrency"] for e in entries]

        for pct in ["p50", "p95", "p99"]:
            vals = [e[pct] for e in entries]
            ax.plot(concs, vals, "o-", label=pct, color=PCT_COLORS[pct])

        ax.set_xlabel("Concurrency")
        ax.set_ylabel("Latency (ms)")
        ax.set_title(f"{tier}")
        ax.legend()
        ax.set_xticks(concs)

    fig.suptitle("Latency Distribution vs Concurrency", fontsize=14, y=1.02)
    fig.tight_layout()
    fig.savefig(os.path.join(PLOTS_DIR, "latency.png"), dpi=150, bbox_inches="tight")
    print(f"  -> plots/latency.png")


def plot_throughput(summary):
    """All tiers on one plot: rps vs concurrency."""
    fig, ax = plt.subplots(figsize=(8, 5))

    for tier in sorted(summary.keys()):
        entries = summary[tier]
        concs = [e["concurrency"] for e in entries]
        rps = [e["rps"] for e in entries]
        ax.plot(concs, rps, marker=TIER_MARKERS.get(tier, "o"), label=tier,
                color=TIER_COLORS.get(tier, "#333"))

    ax.set_xlabel("Concurrency")
    ax.set_ylabel("Requests / sec")
    ax.set_title("Throughput vs Concurrency")
    ax.legend()

    # Use log scale if range spans > 2 orders of magnitude
    all_rps = [e["rps"] for t in summary.values() for e in t if e["rps"] > 0]
    if all_rps and max(all_rps) / max(min(all_rps), 0.001) > 100:
        ax.set_yscale("log")
        ax.yaxis.set_major_formatter(ticker.ScalarFormatter())

    fig.tight_layout()
    fig.savefig(os.path.join(PLOTS_DIR, "throughput.png"), dpi=150, bbox_inches="tight")
    print(f"  -> plots/throughput.png")


def plot_errors(summary):
    """Error rate (%) vs concurrency, one line per tier."""
    fig, ax = plt.subplots(figsize=(8, 5))

    for tier in sorted(summary.keys()):
        entries = summary[tier]
        concs = [e["concurrency"] for e in entries]
        err_pct = [e["error_rate"] * 100 for e in entries]
        ax.plot(concs, err_pct, "o-", label=tier,
                color=TIER_COLORS.get(tier, "#333"))

    ax.set_xlabel("Concurrency")
    ax.set_ylabel("Error Rate (%)")
    ax.set_title("Error Rate vs Concurrency")
    ax.set_ylim(bottom=0)
    ax.legend()

    fig.tight_layout()
    fig.savefig(os.path.join(PLOTS_DIR, "error_rate.png"), dpi=150, bbox_inches="tight")
    print(f"  -> plots/error_rate.png")


def main():
    os.makedirs(PLOTS_DIR, exist_ok=True)
    setup_style()
    summary = load_summary()

    print("Generating plots...")
    plot_latency(summary)
    plot_throughput(summary)
    plot_errors(summary)
    print("Done.")


if __name__ == "__main__":
    main()
