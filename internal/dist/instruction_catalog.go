package dist

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	instructionCatalogExportSchema = "rice-agent/instruction-catalog-export/v1"
	catalogResolutionSchema        = "catalog/resolve-result/v1"
	catalogPacketSchema            = "catalog/context-packet/v1"
)

type instructionCatalogExport struct {
	Schema     string            `json:"schema"`
	Resolution catalogResolution `json:"resolution"`
	Packet     json.RawMessage   `json:"packet"`
}

type catalogResolution struct {
	Schema           string            `json:"schema"`
	Status           string            `json:"status"`
	ResolverVersion  string            `json:"resolverVersion"`
	InputDigest      string            `json:"inputDigest"`
	PacketDigest     string            `json:"packetDigest"`
	MissingMandatory []json.RawMessage `json:"missingMandatory"`
}

type catalogPacket struct {
	Schema          string              `json:"schema"`
	ResolverVersion string              `json:"resolverVersion"`
	InputDigest     string              `json:"inputDigest"`
	Notice          string              `json:"notice"`
	Estimate        json.RawMessage     `json:"estimate"`
	DiscoveryMode   string              `json:"discoveryMode"`
	Items           []catalogPacketItem `json:"items"`
}

type catalogPacketItem struct {
	Order          int                  `json:"order"`
	Ref            catalogEntryRef      `json:"ref"`
	Kind           string               `json:"kind"`
	Classification string               `json:"classification"`
	Title          string               `json:"title"`
	Digest         string               `json:"digest"`
	Provenance     catalogProvenance    `json:"provenance"`
	Text           *string              `json:"text"`
	Assets         []catalogPacketAsset `json:"assets"`
	Capability     *catalogCapability   `json:"capability"`
	Form           string               `json:"form"`
	Label          string               `json:"label"`
}

type catalogCapability struct {
	ReferenceOnly bool                         `json:"referenceOnly"`
	Definition    *catalogCapabilityDefinition `json:"definition"`
}

type catalogCapabilityDefinition struct {
	Digest string `json:"digest"`
}

type catalogEntryRef struct {
	Pack     string `json:"pack"`
	Revision string `json:"revision"`
	Entry    string `json:"entry"`
}

type catalogProvenance struct {
	PackDigest string `json:"packDigest"`
}

type catalogPacketAsset struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

func catalogExportFromFiles(resultBytes, packetBytes []byte) (instructionCatalogExport, error) {
	var result catalogResolution
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		return instructionCatalogExport{}, fmt.Errorf("decode Catalog resolve result: %w", err)
	}
	return catalogExport(result, packetBytes)
}

func catalogExportFromBytes(exportBytes []byte) (instructionCatalogExport, error) {
	var export instructionCatalogExport
	if err := json.Unmarshal(exportBytes, &export); err != nil {
		return instructionCatalogExport{}, fmt.Errorf("decode Catalog export: %w", err)
	}
	if err := validateCatalogExport(export); err != nil {
		return instructionCatalogExport{}, err
	}
	return export, nil
}

func catalogExport(result catalogResolution, packetBytes []byte) (instructionCatalogExport, error) {
	export := instructionCatalogExport{
		Schema:     instructionCatalogExportSchema,
		Resolution: result,
		Packet:     append(json.RawMessage(nil), packetBytes...),
	}
	if err := validateCatalogExport(export); err != nil {
		return instructionCatalogExport{}, err
	}
	return export, nil
}

func validateCatalogExport(export instructionCatalogExport) error {
	if export.Schema != instructionCatalogExportSchema {
		return errors.New("Catalog export has an unsupported Rice Agent schema")
	}
	result := export.Resolution
	if result.Schema != catalogResolutionSchema || result.Status != "complete" {
		return errors.New("Catalog resolution is not complete")
	}
	if result.MissingMandatory == nil || len(result.MissingMandatory) != 0 {
		return errors.New("Catalog resolution has missing mandatory guidance")
	}
	if result.ResolverVersion == "" || !validCatalogDigest(result.InputDigest) || !validCatalogDigest(result.PacketDigest) {
		return errors.New("Catalog resolve result has invalid lineage metadata")
	}
	if len(export.Packet) == 0 || !json.Valid(export.Packet) {
		return errors.New("Catalog context packet is missing or invalid JSON")
	}
	if catalogDigest(export.Packet) != result.PacketDigest {
		return errors.New("Catalog context packet digest does not match the resolve result")
	}
	var packet catalogPacket
	if err := json.Unmarshal(export.Packet, &packet); err != nil {
		return fmt.Errorf("decode Catalog context packet: %w", err)
	}
	if packet.Schema != catalogPacketSchema || packet.DiscoveryMode != "registered" {
		return errors.New("Catalog context packet must use the registered v1 format")
	}
	if packet.ResolverVersion != result.ResolverVersion || packet.InputDigest != result.InputDigest {
		return errors.New("Catalog context packet lineage does not match the resolve result")
	}
	if packet.Notice == "" || len(packet.Estimate) == 0 || packet.Items == nil {
		return errors.New("Catalog context packet is missing required content")
	}
	for i, item := range packet.Items {
		if item.Order != i+1 {
			return fmt.Errorf("Catalog entry %d has a noncanonical order", i+1)
		}
		if item.Ref.Pack == "" || item.Ref.Revision == "" || item.Ref.Entry == "" {
			return fmt.Errorf("Catalog entry %d has incomplete provenance", i+1)
		}
		if item.Form != "full" || item.Label != "pack-content" {
			return fmt.Errorf("Catalog entry %d is not full pack content", i+1)
		}
		if item.Kind != "rule" && item.Kind != "skill" && item.Kind != "plan-template" && item.Kind != "mcp-capability-reference" {
			return fmt.Errorf("Catalog entry %d has an unsupported kind", i+1)
		}
		if !validCatalogDigest(item.Digest) || !validCatalogDigest(item.Provenance.PackDigest) {
			return fmt.Errorf("Catalog entry %d has invalid content lineage", i+1)
		}
		if item.Kind == "mcp-capability-reference" {
			if item.Capability == nil || !item.Capability.ReferenceOnly {
				return fmt.Errorf("Catalog entry %d is not a reference-only capability", i+1)
			}
			if item.Capability.Definition != nil && item.Capability.Definition.Digest != "" && !validCatalogDigest(item.Capability.Definition.Digest) {
				return fmt.Errorf("Catalog entry %d has invalid capability definition lineage", i+1)
			}
			continue
		}
		if item.Text == nil || catalogDigest([]byte(*item.Text)) != item.Digest {
			return fmt.Errorf("Catalog entry %d content digest does not match", i+1)
		}
		for _, asset := range item.Assets {
			if asset.Path == "" || !validCatalogDigest(asset.Digest) {
				return fmt.Errorf("Catalog entry %d has invalid asset lineage", i+1)
			}
		}
	}
	return nil
}

func validCatalogDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && value == strings.ToLower(value)
}

func catalogDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func marshalCatalogExport(export instructionCatalogExport) ([]byte, error) {
	if err := validateCatalogExport(export); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(export); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func renderCatalogGuidance(export instructionCatalogExport) ([]byte, error) {
	if err := validateCatalogExport(export); err != nil {
		return nil, err
	}
	var packet catalogPacket
	if err := json.Unmarshal(export.Packet, &packet); err != nil {
		return nil, err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# Catalog resolved guidance\n\nResolver: `%s`\n\nInput digest: `%s`\n\nPacket digest: `%s`\n", export.Resolution.ResolverVersion, export.Resolution.InputDigest, export.Resolution.PacketDigest)
	for _, item := range packet.Items {
		if item.Kind == "mcp-capability-reference" || (item.Kind == "skill" && len(item.Assets) != 0) {
			continue
		}
		fmt.Fprintf(&out, "\n## %s\n\nPack: `%s` revision `%s`, entry `%s`\n\nClassification: `%s`\n\nEntry digest: `%s`\n\nPack digest: `%s`\n\n", markdownInline(item.Title), markdownInline(item.Ref.Pack), markdownInline(item.Ref.Revision), markdownInline(item.Ref.Entry), markdownInline(item.Classification), item.Digest, item.Provenance.PackDigest)
		out.WriteString(*item.Text)
		if !strings.HasSuffix(*item.Text, "\n") {
			out.WriteByte('\n')
		}
	}
	return []byte(out.String()), nil
}

func markdownInline(value string) string {
	value = strings.NewReplacer("\r", " ", "\n", " ", "`", "\\`").Replace(value)
	return strings.TrimSpace(value)
}
