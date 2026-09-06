#!/usr/bin/env python3
"""Validate the Git-managed registry against its JSON Schemas.

The registry is the half of FirmScout's dataset that humans write by hand
(ADR-0016), so it is the half that needs a fast, readable failure. This script is
what CI runs on every pull request touching `dataset/` or `collectors/config/`.

The Go loaders remain the authority on what a valid document means; these schemas
exist so a contributor learns about a typo in seconds rather than after a database
round trip.
"""
import glob
import json
import sys

try:
    import yaml
except ImportError:
    sys.exit("PyYAML is required: pip install pyyaml")
try:
    import jsonschema
except ImportError:
    sys.exit("jsonschema is required: pip install jsonschema")

PAIRS = [
    ("packages/schemas/vendor.schema.json", "dataset/vendors/*.yaml"),
    ("packages/schemas/category.schema.json", "dataset/categories.yaml"),
    # Families live in a tree of their own rather than under dataset/products/,
    # because the product pattern below matches recursively: a family document filed
    # beside the devices it groups would be validated against product.schema.json and
    # fail CI for the wrong reason.
    ("packages/schemas/family.schema.json", "dataset/families/**/*.yaml"),
    ("packages/schemas/product.schema.json", "dataset/products/**/*.yaml"),
    ("packages/schemas/source.schema.json", "dataset/sources/**/*.yaml"),
    ("packages/schemas/collector-config.schema.json", "collectors/config/**/*.yaml"),
]


def main() -> int:
    failures = 0
    checked = 0
    for schema_path, pattern in PAIRS:
        schema = json.load(open(schema_path))
        validator = jsonschema.Draft202012Validator(schema)
        for path in sorted(glob.glob(pattern, recursive=True)):
            checked += 1
            # YAML turns timestamps into datetime objects; the schemas describe the
            # serialised form, so normalise through JSON first.
            # A file may hold several documents; the category vocabulary does.
            documents = [d for d in yaml.safe_load_all(open(path)) if d is not None]
            errors = []
            for document in documents:
                document = json.loads(json.dumps(document, default=str))
                errors.extend(sorted(validator.iter_errors(document), key=lambda e: list(e.path)))
            if not errors:
                print(f"ok   {path} ({len(documents)} document(s))")
                continue
            failures += 1
            print(f"FAIL {path}")
            for error in errors[:8]:
                where = "/".join(str(p) for p in error.absolute_path) or "(root)"
                print(f"       {where}: {error.message}")

    print()
    print(f"Checked {checked} registry document(s).")
    if failures:
        print(f"RESULT: FAILED ({failures} invalid)")
        return 1
    print("RESULT: PASSED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
