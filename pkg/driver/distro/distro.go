// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2023 The Falco Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package driverdistro implements all the distro specific driver-related logic.
package driverdistro

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/docker/docker/pkg/homedir"
	"github.com/falcosecurity/driverkit/pkg/kernelrelease"
	"golang.org/x/net/context"
	"gopkg.in/ini.v1"

	"github.com/falcosecurity/falcoctl/internal/utils"
	drivertype "github.com/falcosecurity/falcoctl/pkg/driver/type"
	"github.com/falcosecurity/falcoctl/pkg/output"
)

const (
	// DefaultFalcoRepo is the default repository provided by falcosecurity to download driver artifacts from.
	kernelDirEnv            = "KERNELDIR"
	kernelSrcDownloadFolder = "kernel-sources"
)

var distros = map[string]Distro{}

// ErrUnsupported is the error returned when the target distro is not supported.
var ErrUnsupported = errors.New("failed to determine distro")

// Distro is the common interface used by distro-specific implementations.
// Most of the distro-specific only partially override the default `generic` implementation.
type Distro interface {
	init(kr kernelrelease.KernelRelease, id string, cfg *ini.File) error    // private
	FixupKernel(kr kernelrelease.KernelRelease) kernelrelease.KernelRelease // private
	customizeBuild(ctx context.Context, printer *output.Printer, driverType drivertype.DriverType,
		kr kernelrelease.KernelRelease, hostRoot string) (map[string]string, error)
	PreferredDriver(kr kernelrelease.KernelRelease) drivertype.DriverType
	fmt.Stringer
}

type checker interface {
	check(hostRoot string) bool // private
}

// DiscoverDistro tries to fetch the correct Distro by looking at /etc/os-release or
// by cycling on all supported distros and checking them one by one.
//
//nolint:gocritic // the method shall not be able to modify kr
func DiscoverDistro(kr kernelrelease.KernelRelease, hostRoot string) (Distro, error) {
	distro, err := getOSReleaseDistro(&kr, hostRoot)
	if err == nil {
		return distro, nil
	}

	// Fallback to check any distro checker
	for id, d := range distros {
		dd, ok := d.(checker)
		if ok && dd.check(hostRoot) {
			err = d.init(kr, id, nil)
			return d, err
		}
	}

	// Return a generic distro to try the build
	distro = &generic{}
	if err = distro.init(kr, "undetermined", nil); err != nil {
		return nil, err
	}
	return distro, ErrUnsupported
}

func getOSReleaseDistro(kr *kernelrelease.KernelRelease, hostRoot string) (Distro, error) {
	cfg, err := ini.Load(hostRoot + "/etc/os-release")
	if err != nil {
		return nil, err
	}
	idKey := cfg.Section("").Key("ID")
	if idKey == nil {
		// OS-release without `ID` (can it happen?)
		return nil, nil
	}
	id := strings.ToLower(idKey.String())

	// Overwrite the OS_ID if /etc/VERSION file is present.
	// Not sure if there is a better way to detect minikube.
	dd := distros["minikube"].(checker)
	if dd.check(hostRoot) {
		id = "minikube"
	}

	distro, exist := distros[id]
	if !exist {
		distro = &generic{}
	}
	if err = distro.init(*kr, id, cfg); err != nil {
		return nil, err
	}
	return distro, nil
}

func toURL(repo, driverVer, fileName, arch string) string {
	return fmt.Sprintf("%s/%s/%s/%s", repo, driverVer, arch, fileName)
}

func toLocalPath(driverVer, fileName, arch string) string {
	return fmt.Sprintf("%s/.falco/%s/%s/%s", homedir.Get(), driverVer, arch, fileName)
}

func toFilename(d Distro, kr *kernelrelease.KernelRelease, driverName string, driverType drivertype.DriverType) string {
	// Fixup kernel information before attempting to download
	fixedKR := d.FixupKernel(*kr)
	return fmt.Sprintf("%s_%s_%s_%s%s", driverName, d, fixedKR.String(), fixedKR.KernelVersion, driverType.Extension())
}

func copyDataToLocalPath(destination string, src io.Reader) error {
	err := os.MkdirAll(filepath.Dir(destination), 0o750)
	if err != nil {
		return err
	}
	out, err := os.Create(filepath.Clean(destination))
	if err == nil {
		defer out.Close()
		_, err = io.Copy(out, src)
	}
	return err
}

// Build will try to build the desired driver for the specified distro and kernel release.
//
//nolint:gocritic // the method shall not be able to modify kr
func Build(ctx context.Context,
	d Distro,
	printer *output.Printer,
	kr kernelrelease.KernelRelease,
	driverName string,
	driverType drivertype.DriverType,
	driverVer string,
	hostRoot string,
) (string, error) {
	env, err := d.customizeBuild(ctx, printer, driverType, kr, hostRoot)
	if err != nil {
		return "", err
	}
	path, err := driverType.Build(ctx, printer, d.FixupKernel(kr), driverName, driverVer, env)
	if err != nil {
		return "", err
	}
	// Copy the path to the expected location.
	// NOTE: for kmod, this is not useful since the driver will
	// be loaded directly by dkms.
	driverFileName := toFilename(d, &kr, driverName, driverType)
	filePath := toLocalPath(driverVer, driverFileName, kr.Architecture.ToNonDeb())
	printer.Logger.Info("Copying built driver to its destination.", printer.Logger.Args("src", path, "dst", filePath))
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer f.Close()
	return filePath, copyDataToLocalPath(filePath, f)
}

// Download will try to download drivers for a distro trying specified repos.
//
//nolint:gocritic // the method shall not be able to modify kr
func Download(ctx context.Context,
	d Distro,
	printer *output.Printer,
	kr kernelrelease.KernelRelease,
	driverName string,
	driverType drivertype.DriverType,
	driverVer string, repos []string,
) (string, error) {
	driverFileName := toFilename(d, &kr, driverName, driverType)
	// Skip if existent
	destination := toLocalPath(driverVer, driverFileName, kr.Architecture.ToNonDeb())
	if exist, _ := utils.FileExists(destination); exist {
		printer.Logger.Info("Skipping download, driver already present.", printer.Logger.Args("path", destination))
		return destination, nil
	}

	// Try to download from any specified repository,
	// stopping at first successful http GET.
	for _, repo := range repos {
		url := toURL(repo, driverVer, driverFileName, kr.Architecture.ToNonDeb())
		printer.Logger.Info("Trying to download a driver.", printer.Logger.Args("url", url))

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			printer.Logger.Warn("Error creating http request.", printer.Logger.Args("err", err))
			continue
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			if err == nil {
				_ = resp.Body.Close()
			}
			printer.Logger.Warn("Error GETting url.", printer.Logger.Args("err", err))
			continue
		}
		return destination, copyDataToLocalPath(destination, resp.Body)
	}
	return destination, fmt.Errorf("unable to find a prebuilt driver")
}

func customizeDownloadKernelSrcBuild(printer *output.Printer, kr *kernelrelease.KernelRelease) error {
	printer.Logger.Info("Configuring kernel.")
	if kr.Extraversion != "" {
		err := utils.ReplaceLineInFile(".config", "LOCALVERSION=", "LOCALVERSION="+kr.Extraversion, 1)
		if err != nil {
			return err
		}
	}
	_, err := exec.Command("bash", "-c", "make olddefconfig").Output()
	if err == nil {
		_, err = exec.Command("bash", "-c", "make modules_prepare").Output()
	}
	return err
}

func getKernelConfig(printer *output.Printer, kr *kernelrelease.KernelRelease, hostRoot string) (string, error) {
	bootConfig := fmt.Sprintf("/boot/config-%s", kr.String())
	hrBootConfig := fmt.Sprintf("%s%s", hostRoot, bootConfig)
	ostreeConfig := fmt.Sprintf("/usr/lib/ostree-boot/config-%s", kr.String())
	hrostreeConfig := fmt.Sprintf("%s%s", hostRoot, ostreeConfig)
	libModulesConfig := fmt.Sprintf("/lib/modules/%s/config", kr.String())

	toBeChecked := []string{
		"/proc/config.gz",
		bootConfig,
		hrBootConfig,
		ostreeConfig,
		hrostreeConfig,
		libModulesConfig,
	}

	for _, path := range toBeChecked {
		if exist, _ := utils.FileExists(path); exist {
			printer.Logger.Info("Found kernel config.", printer.Logger.Args("path", path))
			return path, nil
		}
	}
	return "", fmt.Errorf("cannot find kernel config")
}

func downloadKernelSrc(ctx context.Context,
	printer *output.Printer,
	kr *kernelrelease.KernelRelease,
	url string, hostRoot string,
	stripComponents int,
) (map[string]string, error) {
	env := make(map[string]string)

	printer.Logger.Info("Downloading kernel sources.", printer.Logger.Args("url", url))
	err := os.MkdirAll("/tmp/kernel", 0o750)
	if err != nil {
		return env, err
	}
	tempDir, err := os.MkdirTemp("/tmp/kernel", "")
	if err != nil {
		return env, err
	}

	// Download the url
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return env, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return env, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return env, fmt.Errorf("non-200 http GET status code")
	}

	printer.Logger.Info("Extracting kernel sources.")

	fullKernelDir := filepath.Join(tempDir, kernelSrcDownloadFolder)

	err = os.Mkdir(fullKernelDir, 0o750)
	if err != nil {
		return env, err
	}

	_, err = utils.ExtractTarGz(resp.Body, fullKernelDir, stripComponents)
	if err != nil {
		return env, err
	}

	kernelConfigPath, err := getKernelConfig(printer, kr, hostRoot)
	if err != nil {
		return nil, err
	}
	dest, err := os.Create(".config")
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Clean(kernelConfigPath))
	if err != nil {
		return nil, err
	}
	var src io.Reader
	if strings.HasSuffix(kernelConfigPath, ".gz") {
		src = tar.NewReader(f)
	} else {
		src = f
	}
	fStat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	_, err = io.CopyN(dest, src, fStat.Size())
	if err != nil {
		return nil, err
	}
	env[kernelDirEnv] = fullKernelDir
	return env, nil
}
