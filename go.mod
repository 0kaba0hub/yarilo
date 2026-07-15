module github.com/0kaba0hub/yarilo

go 1.26.2

require (
	github.com/GehirnInc/crypt v0.0.0-20230320061759-8cc1b52080c5
	github.com/MicahParks/keyfunc/v3 v3.8.0
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/emersion/go-imap/v2 v2.0.0-beta.8.0.20260619144330-06898882abab
	github.com/emersion/go-message v0.18.2
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6
	github.com/emersion/go-smtp v0.24.0
	github.com/foxcpp/go-sieve v0.0.0-20260703081245-75bf8d082267
	github.com/go-sql-driver/mysql v1.10.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/jackc/pgx/v5 v5.10.0
	github.com/knadh/koanf/parsers/yaml v1.1.0
	github.com/knadh/koanf/providers/file v1.2.1
	github.com/knadh/koanf/v2 v2.3.5
	github.com/pires/go-proxyproto v0.15.0
	github.com/prometheus/client_golang v1.23.2
	github.com/redis/go-redis/v9 v9.21.0
	golang.org/x/crypto v0.54.0
	golang.org/x/text v0.40.0
	modernc.org/sqlite v1.53.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/MicahParks/jwkset v0.11.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/knadh/koanf/maps v0.1.2 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.0 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rogpeppe/go-internal v1.15.0 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	rsc.io/binaryregexp v0.2.0 // indirect
)

replace github.com/foxcpp/go-sieve => github.com/0kaba0hub/go-sieve v0.0.0-20260713145935-39e12c5d4b1f

replace github.com/emersion/go-imap/v2 => github.com/0kaba0hub/go-imap/v2 v2.0.0-beta.8.0.20260715021749-966cc7248645

replace github.com/emersion/go-sasl => github.com/0kaba0hub/go-sasl v0.0.0-20260603191939-ef5a2942848f

replace github.com/emersion/go-smtp => github.com/0kaba0hub/go-smtp v0.0.0-20260712165350-106ee832679f
