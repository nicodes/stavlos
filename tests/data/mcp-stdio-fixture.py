"""Offline test server. It has no provider integration and makes no network calls."""
import json, os, pathlib, subprocess, sys

root = pathlib.Path(sys.argv[1])
mode = sys.argv[2] if len(sys.argv) > 2 else "ordinary"
root.joinpath("server.pid").write_text(str(os.getpid()))
if mode == "fail":
    sys.stderr.write("fixture failed: " + "x" * 5000)
    sys.stderr.flush()
    sys.exit(23)

def respond(request, result):
    print(json.dumps({"jsonrpc": "2.0", "id": request["id"], "result": result}), flush=True)

for line in sys.stdin:
    request = json.loads(line)
    method = request.get("method", "")
    if method == "notifications/cancelled":
        root.joinpath("cancelled.json").write_text(json.dumps(request["params"]))
    if mode == "quiet":
        continue
    if method == "initialize":
        respond(request, {"protocolVersion": request["params"]["protocolVersion"], "capabilities": {"tools": {}}, "serverInfo": {"name": "offline-fixture", "version": "1"}})
    elif method == "tools/list":
        names = ["echo"] if not request.get("params", {}).get("cursor") else ["hang", "exit", "child", "echo"]
        result = {"tools": [{"name": name, "title": "Title " + name, "inputSchema": {"type": "object"}} for name in names]}
        if names == ["echo"]:
            result["nextCursor"] = "next"
        respond(request, result)
    elif method == "tools/call":
        name = request["params"]["name"]
        if name == "exit":
            os._exit(7)
        if name == "hang":
            continue
        if name == "child":
            child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"])
            text = str(child.pid)
        else:
            text = json.dumps({"arguments": request["params"].get("arguments"), "configured": os.getenv("MCP_FIXTURE_TOKEN"), "stage": os.getenv("STAVLOS_SANDBOX_STAGE"), "spec": os.getenv("STAVLOS_SANDBOX_SPEC")}, sort_keys=True)
        respond(request, {"content": [{"type": "text", "text": text}]})
