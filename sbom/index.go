/*
 * Copyright © 2022 Docker, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package sbom

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/atomist-skills/go-skill"
	"github.com/docker/docker/client"
	"github.com/docker/index-cli-plugin/internal"
	"github.com/docker/index-cli-plugin/query"
	"github.com/docker/index-cli-plugin/registry"
	"github.com/docker/index-cli-plugin/types"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/pkg/errors"
)

type ImageIndexResult struct {
	Input string
	Image *v1.Image
	Sbom  *types.Sbom
	Error error
}

func indexImageAsync(wg *sync.WaitGroup, image string, client client.APIClient, resultChan chan<- ImageIndexResult) {
	defer wg.Done()
	sbom, img, err := IndexImage(image, client)
	cves, err := query.QueryCves(sbom, "", "", "")
	if err == nil {
		sbom.Vulnerabilities = *cves
	}
	resultChan <- ImageIndexResult{
		Input: image,
		Image: img,
		Sbom:  sbom,
		Error: err,
	}
}

func IndexPath(path string, name string) (*types.Sbom, *v1.Image, error) {
	skill.Log.Infof("Loading image from %s", path)
	img, err := registry.ReadImage(path)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to read image")
	}
	skill.Log.Infof("Loaded image")
	return indexImage(img, name, path)
}

func IndexImage(image string, client client.APIClient) (*types.Sbom, *v1.Image, error) {
	skill.Log.Infof("Copying image %s", image)
	img, path, err := registry.SaveImage(image, client)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to download image")
	}
	skill.Log.Infof("Copied image")
	return indexImage(img, image, path)
}

func indexImage(img v1.Image, imageName, path string) (*types.Sbom, *v1.Image, error) {
	// see if we can re-use an existing sbom
	sbomPath := filepath.Join(path, "sbom.json")
	if _, ok := os.LookupEnv("ATOMIST_NO_CACHE"); !ok {
		if _, err := os.Stat(sbomPath); !os.IsNotExist(err) {
			var sbom types.Sbom
			b, err := os.ReadFile(sbomPath)
			if err == nil {
				err := json.Unmarshal(b, &sbom)
				if err == nil {
					if sbom.Descriptor.SbomVersion == internal.FromBuild().SbomVersion && sbom.Descriptor.Version == internal.FromBuild().Version {
						skill.Log.Infof(`Indexed %d packages`, len(sbom.Artifacts))
						return &sbom, &img, nil
					}
				}
			}
		}
	}

	lm := createLayerMapping(img)
	skill.Log.Debugf("Created layer mapping")

	skill.Log.Info("Indexing")
	trivyResultChan := make(chan types.IndexResult)
	syftResultChan := make(chan types.IndexResult)
	go trivySbom(path, lm, trivyResultChan)
	go syftSbom(path, lm, syftResultChan)

	trivyResult := <-trivyResultChan
	syftResult := <-syftResultChan

	var err error
	trivyResult.Packages, err = types.NormalizePackages(trivyResult.Packages)
	syftResult.Packages, err = types.NormalizePackages(syftResult.Packages)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "failed to normalize packagess: %s", imageName)
	}

	packages := types.MergePackages(syftResult, trivyResult)

	skill.Log.Infof(`Indexed %d packages`, len(packages))

	manifest, _ := img.RawManifest()
	config, _ := img.RawConfigFile()
	c, _ := img.ConfigFile()
	m, _ := img.Manifest()
	d, _ := img.Digest()

	var tag []string
	if imageName != "" {
		ref, err := name.ParseReference(imageName)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "failed to parse reference: %s", imageName)
		}
		imageName = ref.Context().String()
		if !strings.HasPrefix(ref.Identifier(), "sha256:") {
			tag = []string{ref.Identifier()}
		}
	}

	sbom := types.Sbom{
		Artifacts: packages,
		Source: types.Source{
			Type: "image",
			Image: types.ImageSource{
				Name:        imageName,
				Digest:      d.String(),
				Manifest:    m,
				Config:      c,
				RawManifest: base64.StdEncoding.EncodeToString(manifest),
				RawConfig:   base64.StdEncoding.EncodeToString(config),
				Distro:      syftResult.Distro,
				Platform: types.Platform{
					Os:           c.OS,
					Architecture: c.Architecture,
					Variant:      c.Variant,
				},
				Size: m.Config.Size,
			},
		},
		Descriptor: types.Descriptor{
			Name:        "docker index",
			Version:     internal.FromBuild().Version,
			SbomVersion: internal.FromBuild().SbomVersion,
		},
	}

	if len(tag) > 0 {
		sbom.Source.Image.Tags = &tag
	}

	js, err := json.MarshalIndent(sbom, "", "  ")
	if err == nil {
		_ = os.WriteFile(sbomPath, js, 0644)
	}

	return &sbom, &img, nil
}

func createLayerMapping(img v1.Image) types.LayerMapping {
	lm := types.LayerMapping{
		ByDiffId:        make(map[string]string, 0),
		ByDigest:        make(map[string]string, 0),
		DiffIdByOrdinal: make(map[int]string, 0),
		DigestByOrdinal: make(map[int]string, 0),
		OrdinalByDiffId: make(map[string]int, 0),
	}
	config, _ := img.ConfigFile()
	diffIds := config.RootFS.DiffIDs
	manifest, _ := img.Manifest()
	layers := manifest.Layers

	for i := range layers {
		layer := layers[i]
		diffId := diffIds[i]

		lm.ByDiffId[diffId.String()] = layer.Digest.String()
		lm.ByDigest[layer.Digest.String()] = diffId.String()
		lm.OrdinalByDiffId[diffId.String()] = i
		lm.DiffIdByOrdinal[i] = diffId.String()
		lm.DigestByOrdinal[i] = layer.Digest.String()
	}

	return lm
}
