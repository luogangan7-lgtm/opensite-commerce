# Business module convention

Follow A1 in `docs/architecture/APP_LAYER_DECISIONS.md`:

```
<name>/contract/                     public contracts and consumer ports
<name>/internal/{service,store,httpapi}/ private implementation
<name>/module.go                     static wiring called by bootstrap
```

Other modules import only `contract/` or approved public ports. Go's `internal`
rule protects implementation imports. Each module owns its tables and validates
domain rules; handlers use platform transport helpers. No empty Go packages are
created here. Registry/import-graph enforcement belongs to C-03d.
