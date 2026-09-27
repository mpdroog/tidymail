#!/usr/bin/env python3
"""Draws the Tidymail app icon as a 1024x1024 PNG: a white envelope on the
UI's green. Standard library only, so the build needs nothing installed."""
import struct
import sys
import zlib

SIZE = 1024
GREEN = (53, 96, 63)
WHITE = (255, 255, 255)
SHADE = (214, 226, 216)


def inside_rounded(x, y, x0, y0, x1, y1, r):
    if x < x0 or x > x1 or y < y0 or y > y1:
        return False
    cx = min(max(x, x0 + r), x1 - r)
    cy = min(max(y, y0 + r), y1 - r)
    return (x - cx) ** 2 + (y - cy) ** 2 <= r * r


def near_line(x, y, ax, ay, bx, by, width):
    dx, dy = bx - ax, by - ay
    t = max(0.0, min(1.0, ((x - ax) * dx + (y - ay) * dy) / (dx * dx + dy * dy)))
    px, py = ax + t * dx, ay + t * dy
    return (x - px) ** 2 + (y - py) ** 2 <= (width / 2) ** 2


def pixel(x, y):
    # macOS icon grid: the tile is inset from the canvas edge.
    if not inside_rounded(x, y, 100, 100, 924, 924, 185):
        return None
    ex0, ey0, ex1, ey1 = 250, 330, 774, 694
    if inside_rounded(x, y, ex0, ey0, ex1, ey1, 36):
        mid = (ex0 + ex1) / 2
        if near_line(x, y, ex0 + 30, ey0 + 30, mid, 540, 34) or near_line(x, y, ex1 - 30, ey0 + 30, mid, 540, 34):
            return GREEN
        return WHITE if y < 600 else SHADE if y > 680 else WHITE
    return GREEN


def main(path):
    rows = bytearray()
    for y in range(SIZE):
        rows.append(0)
        for x in range(SIZE):
            c = pixel(x + 0.5, y + 0.5)
            rows.extend((0, 0, 0, 0) if c is None else (*c, 255))

    def chunk(kind, data):
        body = kind + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body))

    png = b"\x89PNG\r\n\x1a\n"
    png += chunk(b"IHDR", struct.pack(">IIBBBBB", SIZE, SIZE, 8, 6, 0, 0, 0))
    png += chunk(b"IDAT", zlib.compress(bytes(rows), 9))
    png += chunk(b"IEND", b"")
    with open(path, "wb") as f:
        f.write(png)


if __name__ == "__main__":
    main(sys.argv[1])
