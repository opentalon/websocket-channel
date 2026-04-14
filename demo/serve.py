#!/usr/bin/env python3
"""
Serve the WebSocket channel demo UI.

Usage:
    cd /path/to/websocket-channel
    python3 demo/serve.py

Then open http://localhost:8080 in your browser.

Requirements:
    - websocket-channel binary running:
        ./websocket-channel -addr 0.0.0.0:9000 -path /ws
    - OpenTalon running with the websocket channel configured:
        channels:
          - name: websocket
            plugin: ./websocket-channel
            config:
              addr: "0.0.0.0:9000"
              path: "/ws"
"""

import http.server
import os
import socketserver
import sys

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8080
DEMO_DIR = os.path.dirname(os.path.abspath(__file__))

os.chdir(DEMO_DIR)

print(f"Demo UI: http://localhost:{PORT}")
print(f"Serving: {DEMO_DIR}")
print("Press Ctrl+C to stop.\n")

with socketserver.TCPServer(("", PORT), http.server.SimpleHTTPRequestHandler) as httpd:
    httpd.serve_forever()
