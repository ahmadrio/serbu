module serbu

go 1.24

// golang.org/x/* diarahkan ke mirror resmi di GitHub supaya build tetap jalan
// di jaringan yang memblokir golang.org. Isinya identik dengan versi aslinya.
replace golang.org/x/text => github.com/golang/text v0.3.8

replace golang.org/x/sys => github.com/golang/sys v0.0.0-20220715151400-c0bba94af5f8

require (
	github.com/dop251/goja v0.0.0-20260603125802-cfe4039cb6d7
	github.com/evanw/esbuild v0.28.2
	golang.org/x/sys v0.0.0-20220722155257-8c9f86f7a55f
)

require (
	github.com/dlclark/regexp2 v1.11.4 // indirect
	github.com/go-sourcemap/sourcemap v2.1.3+incompatible // indirect
	github.com/google/pprof v0.0.0-20230207041349-798e818bf904 // indirect
	golang.org/x/text v0.3.8 // indirect
)
