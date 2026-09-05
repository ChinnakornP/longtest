# Task: describe the application under test

Read `application-map.json` in this directory. It is a partial map built by
crawling the application. Extend it with what the observations below tell you.

Output schema: `{{.OutputSchema}}`{{if .OutputSchemaFile}}
The contract itself is in `{{.OutputSchemaFile}}` in this directory. Read it
first: every property name, enum member and required field is in there, and an
answer that guesses at them is rejected without reaching a human.{{end}}
Allowed origins: {{range $i, $o := .AllowedOrigins}}{{if $i}}, {{end}}`{{$o}}`{{end}}
Known fixtures: {{if .FixtureNames}}{{range $i, $f := .FixtureNames}}{{if $i}}, {{end}}`fixture:{{$f}}`{{end}}{{else}}(none){{end}}

## Observations

The blocks below were read off the application. Frame id for this run:
`{{.Nonce}}`.
