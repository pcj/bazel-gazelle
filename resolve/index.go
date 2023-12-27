/* Copyright 2018 The Bazel Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resolve

import (
	"log"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/repo"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

// ImportSpec describes a library to be imported. Imp is an import string for
// the library. Lang is the language in which the import string appears (this
// should match Resolver.Name).
type ImportSpec struct {
	Lang, Imp string
}

// Resolver is an interface that language extensions can implement to resolve
// dependencies in rules they generate.
type Resolver interface {
	// Name returns the name of the language. This should be a prefix of the
	// kinds of rules generated by the language, e.g., "go" for the Go extension
	// since it generates "go_library" rules.
	Name() string

	// Imports returns a list of ImportSpecs that can be used to import the rule
	// r. This is used to populate RuleIndex.
	//
	// If nil is returned, the rule will not be indexed. If any non-nil slice is
	// returned, including an empty slice, the rule will be indexed.
	Imports(c *config.Config, r *rule.Rule, f *rule.File) []ImportSpec

	// Embeds returns a list of labels of rules that the given rule embeds. If
	// a rule is embedded by another importable rule of the same language, only
	// the embedding rule will be indexed. The embedding rule will inherit
	// the imports of the embedded rule.
	Embeds(r *rule.Rule, from label.Label) []label.Label

	// Resolve translates imported libraries for a given rule into Bazel
	// dependencies. Information about imported libraries is returned for each
	// rule generated by language.GenerateRules in
	// language.GenerateResult.Imports. Resolve generates a "deps" attribute (or
	// the appropriate language-specific equivalent) for each import according to
	// language-specific rules and heuristics.
	Resolve(c *config.Config, ix *RuleIndex, rc *repo.RemoteCache, r *rule.Rule, imports interface{}, from label.Label)
}

// CrossResolver is an interface that language extensions can implement to provide
// custom dependency resolution logic for other languages.
type CrossResolver interface {
	// CrossResolve attempts to resolve an import string to a rule for languages
	// other than the implementing extension. lang is the langauge of the rule
	// with the dependency.
	CrossResolve(c *config.Config, ix *RuleIndex, imp ImportSpec, lang string) []FindResult
}

// RuleIndex is a table of rules in a workspace, indexed by label and by
// import path. Used by Resolver to map import paths to labels.
type RuleIndex struct {
	rules          []*ruleRecord
	labelMap       map[label.Label]*ruleRecord
	importMap      map[ImportSpec][]*ruleRecord
	mrslv          func(r *rule.Rule, pkgRel string) Resolver
	crossResolvers []CrossResolver
	didFinish      bool
}

// ruleRecord contains information about a rule relevant to import indexing.
type ruleRecord struct {
	rule  *rule.Rule
	label label.Label
	file  *rule.File

	// importedAs is a list of ImportSpecs by which this rule may be imported.
	// Used to build a map from ImportSpecs to ruleRecords.
	importedAs []ImportSpec

	// embeds is the transitive closure of labels for rules that this rule embeds
	// (as determined by the Embeds method). This only includes rules in the same
	// language (i.e., it includes a go_library embedding a go_proto_library, but
	// not a go_proto_library embedding a proto_library).
	embeds []label.Label

	// embedded indicates whether another rule of the same language embeds this
	// rule. Embedded rules should not be indexed.
	embedded bool

	didCollectEmbeds bool

	// lang records the language that this import is relevant for.
	// Due to the presence of mapped kinds, it's otherwise
	// impossible to know the underlying builtin rule type for an
	// arbitrary import.
	lang string
}

// NewRuleIndex creates a new index.
//
// kindToResolver is a map from rule kinds (for example, "go_library") to
// Resolvers that support those kinds.
func NewRuleIndex(mrslv func(r *rule.Rule, pkgRel string) Resolver, exts ...interface{}) *RuleIndex {
	var crossResolvers []CrossResolver
	for _, e := range exts {
		if cr, ok := e.(CrossResolver); ok {
			crossResolvers = append(crossResolvers, cr)
		}
	}
	return &RuleIndex{
		labelMap:       make(map[label.Label]*ruleRecord),
		mrslv:          mrslv,
		crossResolvers: crossResolvers,
	}
}

// AddRule adds a rule r to the index. The rule will only be indexed if there
// is a known resolver for the rule's kind and Resolver.Imports returns a
// non-nil slice.
//
// AddRule may only be called before Finish.
func (ix *RuleIndex) AddRule(c *config.Config, r *rule.Rule, f *rule.File) {
	if ix.didFinish {
		log.Panicf(".AddRule must not be called after .Finish")
	}

	var lang string
	var imps []ImportSpec
	if rslv := ix.mrslv(r, f.Pkg); rslv != nil {
		lang = rslv.Name()
		if passesLanguageFilter(c.Langs, lang) {
			imps = rslv.Imports(c, r, f)
		}
	}
	// If imps == nil, the rule is not importable. If imps is the empty slice,
	// it may still be importable if it embeds importable libraries.
	if imps == nil {
		return
	}

	record := &ruleRecord{
		rule:       r,
		label:      label.New(c.RepoName, f.Pkg, r.Name()),
		file:       f,
		importedAs: imps,
		lang:       lang,
	}
	if _, ok := ix.labelMap[record.label]; ok {
		log.Printf("multiple rules found with label %s", record.label)
		return
	}
	ix.rules = append(ix.rules, record)
	ix.labelMap[record.label] = record
}

// Finish constructs the import index and performs any other necessary indexing
// actions after all rules have been added. This step is necessary because
// a rule may be indexed differently based on what rules are added later.
//
// Finish must be called after all AddRule calls and before any
// FindRulesByImport calls.
func (ix *RuleIndex) Finish() {
	for _, r := range ix.rules {
		ix.collectEmbeds(r)
	}
	ix.buildImportIndex()
	ix.didFinish = true
}

func (ix *RuleIndex) collectEmbeds(r *ruleRecord) {
	if r.didCollectEmbeds {
		return
	}
	resolver := ix.mrslv(r.rule, r.file.Pkg)
	r.didCollectEmbeds = true
	embedLabels := resolver.Embeds(r.rule, r.label)
	r.embeds = embedLabels
	for _, e := range embedLabels {
		er, ok := ix.findRuleByLabel(e, r.label)
		if !ok {
			continue
		}
		ix.collectEmbeds(er)
		erResolver := ix.mrslv(er.rule, er.file.Pkg)
		if resolver.Name() == erResolver.Name() {
			er.embedded = true
			r.embeds = append(r.embeds, er.embeds...)
		}
		r.importedAs = append(r.importedAs, er.importedAs...)
	}
}

// buildImportIndex constructs the map used by FindRulesByImport.
func (ix *RuleIndex) buildImportIndex() {
	ix.importMap = make(map[ImportSpec][]*ruleRecord)
	for _, r := range ix.rules {
		if r.embedded {
			continue
		}
		indexed := make(map[ImportSpec]bool)
		for _, imp := range r.importedAs {
			if indexed[imp] {
				continue
			}
			indexed[imp] = true
			ix.importMap[imp] = append(ix.importMap[imp], r)
		}
	}
}

func (ix *RuleIndex) findRuleByLabel(label label.Label, from label.Label) (*ruleRecord, bool) {
	label = label.Abs(from.Repo, from.Pkg)
	r, ok := ix.labelMap[label]
	return r, ok
}

type FindResult struct {
	// Label is the absolute label (including repository and package name) for
	// a matched rule.
	Label label.Label

	// Embeds is the transitive closure of labels for rules that the matched
	// rule embeds. It may contains duplicates and does not include the label
	// for the rule itself.
	Embeds []label.Label
}

// FindRulesByImport attempts to resolve an import string to a rule record.
// imp is the import to resolve (which includes the target language). lang is
// the language of the rule with the dependency (for example, in
// go_proto_library, imp will have ProtoLang and lang will be GoLang).
// from is the rule which is doing the dependency. This is used to check
// vendoring visibility and to check for self-imports.
//
// FindRulesByImport returns a list of rules, since any number of rules may
// provide the same import. Callers may need to resolve ambiguities using
// language-specific heuristics.
//
// DEPRECATED: use FindRulesByImportWithConfig instead
func (ix *RuleIndex) FindRulesByImport(imp ImportSpec, lang string) []FindResult {
	matches := ix.importMap[imp]
	results := make([]FindResult, 0, len(matches))
	for _, m := range matches {
		if m.lang != lang {
			continue
		}
		results = append(results, FindResult{
			Label:  m.label,
			Embeds: m.embeds,
		})
	}
	return results
}

// FindRulesByImportWithConfig attempts to resolve an import to a rule first by
// checking the rule index, then if no matches are found any registered
// CrossResolve implementations are called.
func (ix *RuleIndex) FindRulesByImportWithConfig(c *config.Config, imp ImportSpec, lang string) []FindResult {
	results := ix.FindRulesByImport(imp, lang)
	if len(results) > 0 {
		return results
	}
	for _, cr := range ix.crossResolvers {
		results = append(results, cr.CrossResolve(c, ix, imp, lang)...)
	}
	return results
}

// IsSelfImport returns true if the result's label matches the given label
// or the result's rule transitively embeds the rule with the given label.
// Self imports cause cyclic dependencies, so the caller may want to omit
// the dependency or report an error.
func (r FindResult) IsSelfImport(from label.Label) bool {
	if from.Equal(r.Label) {
		return true
	}
	for _, e := range r.Embeds {
		if from.Equal(e) {
			return true
		}
	}
	return false
}

// passesLanguageFilter returns true if the filter is empty (disabled) or if the
// given language name appears in it.
func passesLanguageFilter(langFilter []string, langName string) bool {
	if len(langFilter) == 0 {
		return true
	}
	for _, l := range langFilter {
		if l == langName {
			return true
		}
	}
	return false
}
