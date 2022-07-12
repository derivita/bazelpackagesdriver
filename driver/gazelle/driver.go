package gazelle

import (
	"log"

	"github.com/derivita/bazelpackagesdriver/driver"
	"golang.org/x/tools/go/packages"
)

type gazelleDriver struct {
}

func (d *gazelleDriver) Run(req driver.Request, patterns ...string) (*driver.Response, error) {
	log.Println(patterns)
	log.Printf("\tenv: %#v\n", req.Env)
	log.Printf("\t%d overrides\n", len(req.Overlay))

	cfg := &packages.Config{
		Mode:       req.Mode,
		Env:        append([]string{"GOPACKAGESDRIVER=off"}, req.Env...),
		BuildFlags: req.BuildFlags,
		Tests:      req.Tests,
		Overlay:    req.Overlay,
	}
	var roots []string

	rootsCfg := *cfg
	rootsCfg.Mode = packages.NeedName

	rootsResult, err := packages.Load(&rootsCfg, patterns...)
	if err != nil {
		return nil, err
	}

	for _, pkg := range rootsResult {
		roots = append(roots, pkg.ID)
	}

	result, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, err
	}
	return &driver.Response{
		Packages: result,
		Roots:    roots,
	}, nil
}

func New() driver.Driver {
	var d gazelleDriver
	return d.Run
}
