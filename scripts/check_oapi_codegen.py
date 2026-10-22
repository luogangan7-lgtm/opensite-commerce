#!/usr/bin/env python3
"""Check the pinned strict generator against committed output without changing it.
Depends on Go tool declarations, api/oapi-codegen.yaml and api/openapi.yaml.
Called by check_all/CI; manual generated edits and contract drift always fail,
printing the unified diff to the generated output.
"""
import difflib
import json
from pathlib import Path
import subprocess
import tempfile
import yaml

ROOT = Path(__file__).resolve().parents[1]
OUTPUT = ROOT / "internal/platform/httpapi/generated/api.gen.go"


def main():
    config = yaml.safe_load((ROOT / "api/oapi-codegen.yaml").read_text())
    for option in ("models", "std-http-server", "strict-server", "embedded-spec"):
        if config.get("generate", {}).get(option) is not True:
            raise SystemExit(f"FAIL: required generator option {option} is disabled")
    if config.get("output-options", {}).get("nullable-type") is not True:
        raise SystemExit("FAIL: three-state nullable generation is required")
    module = json.loads(subprocess.check_output(
        ["go", "list", "-m", "-json", "github.com/oapi-codegen/oapi-codegen/v2"], cwd=ROOT))
    if module["Version"] != "v2.8.0" or module.get("Replace"):
        raise SystemExit("FAIL: oapi-codegen must be exactly v2.8.0 without replacement")
    with tempfile.TemporaryDirectory(prefix="oapi-", dir=ROOT / ".work") as tmp:
        candidate = Path(tmp) / "api.gen.go"
        # v2.8.0 config.output takes precedence over CLI -o. Override the
        # temporary config instead, so this gate never repairs a manual edit.
        config["output"] = str(candidate)
        config_path = Path(tmp) / "config.yaml"
        config_path.write_text(yaml.safe_dump(config))
        subprocess.run(["go", "tool", "oapi-codegen", "-config", str(config_path),
                        "api/openapi.yaml"], cwd=ROOT, check=True, timeout=120)
        if not OUTPUT.exists() or candidate.read_bytes() != OUTPUT.read_bytes():
            # Print the exact repair so CI-only integrators can apply it verbatim.
            committed = OUTPUT.read_text().splitlines(True) if OUTPUT.exists() else []
            print("".join(difflib.unified_diff(committed, candidate.read_text().splitlines(True),
                                               "a/internal/platform/httpapi/generated/api.gen.go",
                                               "b/internal/platform/httpapi/generated/api.gen.go")))
            raise SystemExit("FAIL: generated OpenAPI server differs; regenerate and commit api.gen.go")
    print("PASS: oapi-codegen v2.8.0 strict net/http server regeneration is clean")


if __name__ == "__main__":
    (ROOT / ".work").mkdir(exist_ok=True)
    main()
