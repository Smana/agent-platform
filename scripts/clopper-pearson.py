#!/usr/bin/env python3
"""clopper-pearson.py <bad> <n> [confidence=0.95]: the one-sided upper bound on a failure rate.

Prints the p for which P(X <= bad) = 1 - confidence, X ~ Binomial(n, p), found by bisection.
n == 0 prints 1: nothing observed bounds nothing. Standard library only.
"""
import sys
from math import comb


def upper_bound(bad, n, confidence=0.95):
    if n == 0:
        return 1.0
    lo, hi = 0.0, 1.0
    for _ in range(60):
        p = (lo + hi) / 2
        cdf = sum(comb(n, i) * p**i * (1 - p) ** (n - i) for i in range(bad + 1))
        if cdf > 1 - confidence:
            lo = p  # still too likely to see so few failures: the bound is higher
        else:
            hi = p
    return hi


if __name__ == "__main__":
    args = sys.argv[1:]
    if len(args) not in (2, 3):
        sys.exit("usage: clopper-pearson.py <bad> <n> [confidence=0.95]")
    bad, n = int(args[0]), int(args[1])
    if n < 0 or not 0 <= bad <= n:
        sys.exit("need 0 <= bad <= n")
    conf = float(args[2]) if len(args) == 3 else 0.95
    print("1" if n == 0 else "%.6f" % upper_bound(bad, n, conf))
