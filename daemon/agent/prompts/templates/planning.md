# Task: write a test plan

Read `application-map.json` in this directory and produce a test plan covering
the functional, validation, navigation, ui_behavior and error_handling
categories. Prefer element refs from the map over invented locators; when you
must invent one, mark it `unstable: true`.

Output schema: `{{.OutputSchema}}`
Allowed origins: {{range $i, $o := .AllowedOrigins}}{{if $i}}, {{end}}`{{$o}}`{{end}}
Known fixtures: {{if .FixtureNames}}{{range $i, $f := .FixtureNames}}{{if $i}}, {{end}}`fixture:{{$f}}`{{end}}{{else}}(none){{end}}

A plan that names a host outside the allowed origins, or that puts a literal
credential in a step value, is rejected before it runs. That check is not a
suggestion and it is not applied by you — write the plan correctly and it will
pass.

## Observations

The blocks below were read off the application. Frame id for this run:
`{{.Nonce}}`.
