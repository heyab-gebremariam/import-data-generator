package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/generative-ai-go/genai"
	"github.com/joho/godotenv"
	"github.com/xuri/excelize/v2"
	"google.golang.org/api/option"
)

// Constants
const (
	geminiModelName   = "gemini-1.5-flash-latest"
	apiTimeoutSeconds = 60
	apiRetries        = 2
	envAPIKeyName     = "GEMINI_API_KEY"
	inputDir          = "input"
	outputDir         = "output"
)

// GeneratedRow represents a single row of generated data
type GeneratedRow map[string]interface{}

// FieldInfo holds information about a struct field
type FieldInfo struct {
	Type    string
	JSONTag string
}

// StructInfo holds information about a struct and its fields
type StructInfo struct {
	Def      string
	Fields   map[string]FieldInfo // Maps field name to field info
	IsNested bool                 // Indicates if this struct is used as a field in another struct
}

// AIAgent handles the AI model interactions
type AIAgent struct {
	client *genai.Client
	model  string
}

// NewAIAgent creates a new AIAgent instance
func NewAIAgent(apiKey string) (*AIAgent, error) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, fmt.Errorf("failed to create Gemini client: %w", err)
	}
	return &AIAgent{
		client: client,
		model:  geminiModelName,
	}, nil
}

// GenerateData sends the Go struct definitions to the AI and returns generated data
func (agent *AIAgent) GenerateData(structName string, structInfos map[string]StructInfo) ([]GeneratedRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeoutSeconds*time.Second)
	defer cancel()
	genModel := agent.client.GenerativeModel(agent.model)

	// Combine all struct definitions into the prompt
	var allDefs strings.Builder
	for _, info := range structInfos {
		allDefs.WriteString(fmt.Sprintf("%s\n\n", info.Def))
	}

	// Construct a generic prompt for data generation
	prompt := fmt.Sprintf(`Given the following Go struct definitions:

%s

Generate exactly 30 rows of realistic accounting data for the struct named "%s". 
Each row must be a valid JSON object with field names matching the struct's JSON tags (or field names if no JSON tags are present). 
Ensure values are realistic for accounting purposes:
- Use ISO 8601 format (e.g., "2025-05-16T15:00:00Z") for timestamps.
- Use numbers for numeric fields (e.g., amounts, IDs).
- Use strings for text fields (e.g., names, descriptions).
- For phone number fields, include a country code (e.g., "+1", "+44") and a realistic phone number (e.g., "555-1234") using the correct JSON tags.
- Ensure all required fields are present and non-empty unless explicitly optional (e.g., marked with omitempty in JSON tags).
- Ensure numeric values (e.g., amounts) are reasonable for accounting.
- Ensure IDs are unique across all rows where applicable.


Return only a JSON array of 30 objects, no additional text or wrappers.`, allDefs.String(), structName)

	// Generate content with retries
	var resp *genai.GenerateContentResponse
	var err error
	for attempt := 1; attempt <= apiRetries; attempt++ {
		resp, err = genModel.GenerateContent(ctx, genai.Text(prompt))
		if err == nil {
			break
		}
		log.Printf("Attempt %d failed for %s: %v", attempt, structName, err)
		if attempt == apiRetries {
			return nil, fmt.Errorf("failed to generate content for %s after %d attempts: %v", structName, apiRetries, err)
		}
		time.Sleep(time.Second)
	}

	// Parse the generated JSON
	var rows []GeneratedRow
	if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
		part := resp.Candidates[0].Content.Parts[0]
		if textPart, ok := part.(genai.Text); ok {
			// Clean the response to extract valid JSON
			content := string(textPart)
			log.Printf("Raw AI response for %s: %s", structName, content)

			// Remove common wrappers and ensure valid JSON array
			content = strings.TrimSpace(content)
			content = strings.TrimPrefix(content, "```json\n")
			content = strings.TrimPrefix(content, "```\n")
			content = strings.TrimSuffix(content, "\n```")
			content = strings.TrimSpace(content)
			if !strings.HasPrefix(content, "[") || !strings.HasSuffix(content, "]") {
				content = "[" + content + "]"
			}

			log.Printf("Cleaned JSON content for %s: %s", structName, content)

			// Try to unmarshal the cleaned content
			err = json.Unmarshal([]byte(content), &rows)
			if err != nil {
				return nil, fmt.Errorf("failed to parse generated JSON for %s: %v (cleaned content: %s)", structName, err, content)
			}
		} else {
			return nil, fmt.Errorf("unexpected response format for %s", structName)
		}
	} else {
		return nil, fmt.Errorf("no valid response from model for %s", structName)
	}

	// Validate the number of rows
	if len(rows) < 25 || len(rows) > 30 {
		return nil, fmt.Errorf("expected around 30 rows for %s, got %d", structName, len(rows))
	}

	// Validate schema dynamically based on struct fields
	currentStruct := structInfos[structName]
	for i, row := range rows {
		for fieldName, fieldInfo := range currentStruct.Fields {
			jsonTag := fieldInfo.JSONTag
			if jsonTag == "" {
				jsonTag = fieldName
			}
			if _, ok := row[jsonTag]; !ok {
				return nil, fmt.Errorf("row %d for %s missing required field: %s", i+1, structName, jsonTag)
			}

			// Check if the field is a struct and validate nested object
			if _, isStruct := structInfos[fieldInfo.Type]; isStruct {
				if nested, ok := row[jsonTag].(map[string]interface{}); !ok {
					return nil, fmt.Errorf("row %d for %s has invalid %s field: expected nested object, got %v", i+1, structName, jsonTag, row[jsonTag])
				} else {
					// Validate nested fields
					for nestedField, nestedFieldInfo := range structInfos[fieldInfo.Type].Fields {
						nestedJsonTag := nestedFieldInfo.JSONTag
						if nestedJsonTag == "" {
							nestedJsonTag = nestedField
						}
						if _, ok := nested[nestedJsonTag]; !ok && !strings.Contains(nestedFieldInfo.JSONTag, "omitempty") {
							return nil, fmt.Errorf("row %d for %s has invalid %s field: missing nested field %s", i+1, structName, jsonTag, nestedJsonTag)
						}
					}
				}
			}
		}
	}

	return rows, nil
}

// SaveToExcel saves the generated data to an Excel file
func SaveToExcel(structName string, rows []GeneratedRow, outputPath string) error {
	f := excelize.NewFile()
	defer f.Close()

	// Create sheet
	sheet := "Sheet1"
	f.SetSheetName("Sheet1", sheet)

	// Get headers dynamically from the first row
	if len(rows) == 0 {
		return fmt.Errorf("no data to save for %s", structName)
	}
	headers := make([]string, 0)
	for key := range rows[0] {
		headers = append(headers, key)
	}

	// Write headers
	for col, header := range headers {
		cell := fmt.Sprintf("%s1", string(rune('A'+col)))
		f.SetCellValue(sheet, cell, header)
	}

	// Write data
	for rowIdx, row := range rows {
		for colIdx, header := range headers {
			cell := fmt.Sprintf("%s%d", string(rune('A'+colIdx)), rowIdx+2)
			if value, ok := row[header]; ok {
				switch v := value.(type) {
				case float64:
					f.SetCellValue(sheet, cell, v)
				case string:
					f.SetCellValue(sheet, cell, v)
				case map[string]interface{}:
					jsonStr, err := json.Marshal(v)
					if err != nil {
						log.Printf("Failed to marshal nested object for %s, cell %s: %v", structName, cell, err)
						continue
					}
					f.SetCellValue(sheet, cell, string(jsonStr))
				default:
					f.SetCellValue(sheet, cell, fmt.Sprintf("%v", v))
				}
			}
		}
	}

	// Save file
	log.Printf("Saving Excel file to: %s", outputPath)
	return f.SaveAs(outputPath)
}

// extractStructDefs extracts struct definitions and their field types from a Go file
func extractStructDefs(inputPath string) (map[string]StructInfo, error) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, inputPath, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Go file: %v", err)
	}

	structInfos := make(map[string]StructInfo)
	// First pass: Collect all struct definitions
	for _, decl := range node.Decls {
		if genDecl, ok := decl.(*ast.GenDecl); ok && genDecl.Tok == token.TYPE {
			for _, spec := range genDecl.Specs {
				if typeSpec, ok := spec.(*ast.TypeSpec); ok {
					if structType, ok := typeSpec.Type.(*ast.StructType); ok {
						var buf bytes.Buffer
						err := printer.Fprint(&buf, fset, typeSpec)
						if err != nil {
							log.Printf("Failed to print struct %s: %v", typeSpec.Name.Name, err)
							continue
						}
						// Extract field types and JSON tags
						fields := make(map[string]FieldInfo)
						for _, field := range structType.Fields.List {
							for _, name := range field.Names {
								var fieldType string
								switch ft := field.Type.(type) {
								case *ast.Ident:
									fieldType = ft.Name
								case *ast.SelectorExpr:
									if x, ok := ft.X.(*ast.Ident); ok {
										fieldType = fmt.Sprintf("%s.%s", x.Name, ft.Sel.Name)
									}
								default:
									continue
								}
								// Extract JSON tag
								jsonTag := name.Name
								if field.Tag != nil {
									tag := strings.Trim(field.Tag.Value, "`")
									parts := strings.Split(tag, "json:\"")
									if len(parts) > 1 {
										jsonTag = strings.Split(parts[1], "\"")[0]
										jsonTag = strings.Split(jsonTag, ",")[0] // Remove omitempty, etc.
									}
								}
								fields[name.Name] = FieldInfo{
									Type:    fieldType,
									JSONTag: jsonTag,
								}
							}
						}
						structInfos[typeSpec.Name.Name] = StructInfo{
							Def:      buf.String(),
							Fields:   fields,
							IsNested: false,
						}
					}
				}
			}
		}
	}

	// Second pass: Mark nested structs
	for _, info := range structInfos {
		for _, fieldInfo := range info.Fields {
			if _, exists := structInfos[fieldInfo.Type]; exists {
				nestedInfo := structInfos[fieldInfo.Type]
				nestedInfo.IsNested = true
				structInfos[fieldInfo.Type] = nestedInfo
			}
		}
	}

	if len(structInfos) == 0 {
		return nil, fmt.Errorf("no struct definitions found in %s", inputPath)
	}

	return structInfos, nil
}

// ProcessModel processes a single Go file
func (agent *AIAgent) ProcessModel(inputPath, outputDir string) error {
	log.Printf("Processing Go file: %s", inputPath)

	// Extract struct definitions
	structInfos, err := extractStructDefs(inputPath)
	if err != nil {
		return fmt.Errorf("failed to extract structs: %v", err)
	}

	// Process each non-nested struct
	for structName, info := range structInfos {
		if info.IsNested {
			log.Printf("Skipping nested struct: %s", structName)
			continue
		}
		log.Printf("Processing struct: %s", structName)

		// Generate data
		rows, err := agent.GenerateData(structName, structInfos)
		if err != nil {
			log.Printf("Failed to generate data for %s: %v", structName, err)
			continue
		}

		// Create output filename based on struct name
		outputPath := filepath.Join(outputDir, fmt.Sprintf("%s_output.xlsx", structName))

		// Save to Excel
		err = SaveToExcel(structName, rows, outputPath)
		if err != nil {
			log.Printf("Failed to save Excel for %s: %v", structName, err)
			continue
		}

		log.Printf("Generated Excel file: %s", outputPath)
	}

	return nil
}

// ProcessInputFolder processes all Go files in the input folder
func (agent *AIAgent) ProcessInputFolder(inputDir, outputDir string) error {
	log.Printf("Checking input directory: %s", inputDir)

	// Ensure input directory exists
	if _, err := os.Stat(inputDir); os.IsNotExist(err) {
		return fmt.Errorf("input directory does not exist: %s", inputDir)
	}

	// Ensure output directory exists
	log.Printf("Creating output directory: %s", outputDir)
	err := os.MkdirAll(outputDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}

	// Read input directory
	files, err := os.ReadDir(inputDir)
	if err != nil {
		return fmt.Errorf("failed to read input directory: %v", err)
	}

	// Check for Go files
	goFiles := 0
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".go") {
			goFiles++
		}
	}
	if goFiles == 0 {
		return fmt.Errorf("no Go files found in input directory: %s", inputDir)
	}

	log.Printf("Found %d Go files in input directory", goFiles)

	// Process each Go file
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".go") {
			inputPath := filepath.Join(inputDir, file.Name())
			err := agent.ProcessModel(inputPath, outputDir)
			if err != nil {
				log.Printf("Error processing %s: %v", file.Name(), err)
			}
		}
	}

	return nil
}

func loadEnvVars() string {
	// Load .env file
	err := godotenv.Load()
	if err != nil {
		log.Println("Error loading .env file, continuing without it")
	}

	// Get API key
	apiKey := os.Getenv(envAPIKeyName)
	if apiKey == "" {
		log.Fatalf("Environment variable %s is not set", envAPIKeyName)
	}
	return apiKey
}

func main() {
	log.Println("Starting data generator")
	apiKey := loadEnvVars()

	// Initialize AI agent
	agent, err := NewAIAgent(apiKey)
	if err != nil {
		log.Fatalf("Failed to initialize AI agent: %v", err)
	}
	defer agent.client.Close()

	// Process input folder
	err = agent.ProcessInputFolder(inputDir, outputDir)
	if err != nil {
		log.Fatalf("Error processing input folder: %v", err)
	}

	log.Println("Processing completed successfully")
}
