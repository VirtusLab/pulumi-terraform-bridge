// Copyright 2016-2018, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package convert

import (
	"bytes"
	"io"
	"log"
	"os"

	"github.com/hashicorp/hcl/v2"
	"github.com/spf13/afero"

	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tf2pulumi/il"
	hcl2dotnet "github.com/pulumi/pulumi/pkg/v3/codegen/dotnet"
	hcl2go "github.com/pulumi/pulumi/pkg/v3/codegen/go"
	"github.com/pulumi/pulumi/pkg/v3/codegen/hcl2/syntax"
	hcl2nodejs "github.com/pulumi/pulumi/pkg/v3/codegen/nodejs"
	"github.com/pulumi/pulumi/pkg/v3/codegen/pcl"
	hcl2python "github.com/pulumi/pulumi/pkg/v3/codegen/python"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
)

const (
	LanguageTypescript string = "typescript"
	LanguagePulumi     string = "pulumi"
	LanguagePython     string = "python"
	LanguageCSharp     string = "csharp"
	LanguageGo         string = "go"
)

var (
	ValidLanguages = [...]string{LanguageTypescript, LanguagePulumi, LanguagePython, LanguageCSharp, LanguageGo}
)

type Diagnostics struct {
	All   hcl.Diagnostics
	files []*syntax.File
}

func (d *Diagnostics) NewDiagnosticWriter(w io.Writer, width uint, color bool) hcl.DiagnosticWriter {
	return syntax.NewDiagnosticWriter(w, d.files, width, color)
}

// Convert converts a Terraform module at the provided location into a Pulumi module, written to stdout.
func Convert(opts Options) (map[string][]byte, Diagnostics, error) {
	// Set default options where appropriate.
	if opts.Root == nil {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, Diagnostics{}, err
		}
		opts.Root = afero.NewBasePathFs(afero.NewOsFs(), cwd)
	}
	if opts.ProviderInfoSource == nil {
		opts.ProviderInfoSource = il.PluginProviderInfoSource
	}

	// Attempt to load the config as TF11 first. If this succeeds, use TF11 semantics unless either the config
	// or the options specify otherwise.
	generatedFiles, useTF12, tf11Err := convertTF11(opts)
	if !useTF12 {
		if tf11Err != nil {
			return nil, Diagnostics{}, tf11Err
		}
		return generatedFiles, Diagnostics{}, nil
	}

	var tf12Files []*syntax.File
	var diagnostics hcl.Diagnostics

	if tf11Err == nil {
		// Parse the config.
		parser := syntax.NewParser()
		for filename, contents := range generatedFiles {
			err := parser.ParseFile(bytes.NewReader(contents), filename)
			contract.Assert(err == nil)
		}
		if parser.Diagnostics.HasErrors() {
			return nil, Diagnostics{All: parser.Diagnostics, files: parser.Files}, nil
		}
		tf12Files, diagnostics = parser.Files, append(diagnostics, parser.Diagnostics...)
	} else {
		files, diags := parseTF12(opts)
		if !diags.HasErrors() {
			tf12Files, diagnostics = files, append(diagnostics, diags...)
		} else if opts.TerraformVersion != "11" {
			return nil, Diagnostics{All: diags, files: files}, nil
		} else {
			return nil, Diagnostics{}, tf11Err
		}
	}

	tf12Files, program, programDiags, err := convertTF12(tf12Files, opts)
	if err != nil {
		return nil, Diagnostics{}, err
	}

	diagnostics = append(diagnostics, programDiags...)
	if diagnostics.HasErrors() {
		return nil, Diagnostics{All: diagnostics, files: tf12Files}, nil
	}

	switch opts.TargetLanguage {
	case LanguageTypescript:
		tsFiles, genDiags, _ := hcl2nodejs.GenerateProgram(program)
		generatedFiles, diagnostics = tsFiles, append(diagnostics, genDiags...)
	case LanguagePulumi:
		generatedFiles = map[string][]byte{}
		for _, f := range tf12Files {
			generatedFiles[f.Name] = f.Bytes
		}
	case LanguagePython:
		pyFiles, genDiags, _ := hcl2python.GenerateProgram(program)
		generatedFiles, diagnostics = pyFiles, append(diagnostics, genDiags...)
	case LanguageCSharp:
		csFiles, genDiags, _ := hcl2dotnet.GenerateProgram(program)
		generatedFiles, diagnostics = csFiles, append(diagnostics, genDiags...)
	case LanguageGo:
		goFiles, genDiags, _ := hcl2go.GenerateProgram(program)
		generatedFiles, diagnostics = goFiles, append(diagnostics, genDiags...)
	}

	if diagnostics.HasErrors() {
		return nil, Diagnostics{All: diagnostics, files: tf12Files}, nil
	}

	return generatedFiles, Diagnostics{All: diagnostics, files: tf12Files}, nil
}

type Options struct {
	// AllowMissingProperties, if true, allows code-gen to continue even if the input configuration does not include.
	// values for required properties.
	AllowMissingProperties bool
	// AllowMissingProviders, if true, allows code-gen to continue even if resource providers are missing.
	AllowMissingProviders bool
	// AllowMissingVariables, if true, allows code-gen to continue even if the input configuration references missing
	// variables.
	AllowMissingVariables bool
	// AllowMissingComments allows binding to succeed even if there are errors extracting comments from the source.
	AllowMissingComments bool
	// AnnotateNodesWithLocations is true if the generated source code should contain comments that annotate top-level
	// nodes with their original source locations.
	AnnotateNodesWithLocations bool
	// FilterResourceNames, if true, removes the property indicated by ResourceNameProperty from all resources in the
	// graph.
	FilterResourceNames bool
	// ResourceNameProperty sets the key of the resource name property that will be removed if FilterResourceNames is
	// true.
	ResourceNameProperty string
	// Root, when set, overrides the default filesystem used to load the source Terraform module.
	Root afero.Fs
	// Optional package cache.
	PackageCache *pcl.PackageCache
	// Optional plugin host.
	PluginHost plugin.Host
	// Optional Loader.
	Loader schema.Loader
	// Optional source for provider schema information.
	ProviderInfoSource il.ProviderInfoSource
	// Optional logger for diagnostic information.
	Logger *log.Logger
	// SkipResourceTypechecking, if true, allows code-gen to continue even if resource inputs fail to typecheck.
	SkipResourceTypechecking bool
	// The target language.
	TargetLanguage string
	// The target SDK version.
	TargetSDKVersion string
	// The version of Terraform targeteds by the input configuration.
	TerraformVersion string

	// TargetOptions captures any target-specific options.
	TargetOptions interface{}
}

// logf writes a formatted message to the configured logger, if any.
func (o Options) logf(format string, arguments ...interface{}) {
	if o.Logger != nil {
		o.Logger.Printf(format, arguments...)
	}
}
