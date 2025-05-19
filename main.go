package main

import (
	"bytes"
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

	"github.com/brianvoe/gofakeit/v7"
	"github.com/xuri/excelize/v2"
)

// Constants
const (
	inputDir  = "input"
	outputDir = "output"
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

// generateCustomer generates 30 rows of fake customer data using gofakeit
func generateCustomer(structInfos map[string]StructInfo) ([]GeneratedRow, error) {
	rows := make([]GeneratedRow, 30)
	currentStruct := structInfos["Customer"]

	// Seed gofakeit for reproducible results (optional)
	gofakeit.Seed(time.Now().UnixNano())

	for i := 0; i < 30; i++ {
		row := make(GeneratedRow)

		// Generate data for each field in the Customer struct
		for fieldName, fieldInfo := range currentStruct.Fields {
			jsonTag := fieldInfo.JSONTag
			if jsonTag == "" {
				jsonTag = fieldName
			}

			// Handle nested Phone struct
			if fieldInfo.Type == "Phone" {
				nestedRow := make(map[string]interface{})
				for nestedFieldName, nestedFieldInfo := range structInfos["Phone"].Fields {
					nestedJsonTag := nestedFieldInfo.JSONTag
					if nestedJsonTag == "" {
						nestedJsonTag = nestedFieldName
					}

					switch nestedFieldName {
					case "p1":
						// Generate country code (e.g., "+1", "+44")
						nestedRow[nestedJsonTag] = gofakeit.PhoneFormatted()[:2]
					case "p2":
						// Generate phone number without country code
						nestedRow[nestedJsonTag] = gofakeit.PhoneFormatted()[3:] // Skip country code and space
					}
				}
				row[jsonTag] = nestedRow
				continue
			}

			// Generate data for non-nested fields
			switch fieldName {
			case "Name":
				row[jsonTag] = gofakeit.Name()
			case "Type":
				row[jsonTag] = gofakeit.RandomString([]string{"Individual", "Business", "NonProfit"})
			case "CustomerID":
				row[jsonTag] = gofakeit.UUID()
			case "TaxIdNumber":
				row[jsonTag] = gofakeit.Number(100000000, 999999999) // 9-digit tax ID
			}
		}
		rows[i] = row
	}

	// Validate schema
	for i, row := range rows {
		for fieldName, fieldInfo := range currentStruct.Fields {
			jsonTag := fieldInfo.JSONTag
			if jsonTag == "" {
				jsonTag = fieldName
			}
			if _, ok := row[jsonTag]; !ok {
				return nil, fmt.Errorf("row %d missing required field: %s", i+1, jsonTag)
			}
			if fieldInfo.Type == "Phone" {
				if nested, ok := row[jsonTag].(map[string]interface{}); !ok {
					return nil, fmt.Errorf("row %d has invalid phone field: expected nested object, got %v", i+1, row[jsonTag])
				} else {
					for nestedField, nestedFieldInfo := range structInfos["Phone"].Fields {
						nestedJsonTag := nestedFieldInfo.JSONTag
						if nestedJsonTag == "" {
							nestedJsonTag = nestedField
						}
						if _, ok := nested[nestedJsonTag]; !ok {
							return nil, fmt.Errorf("row %d has invalid phone field: missing nested field %s", i+1, nestedJsonTag)
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
				case int:
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
	// Collect all struct definitions
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
										jsonTag = strings.Split(jsonTag, ",")[0]
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

	// Mark nested structs
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

// processCustomer processes the customer.go file and generates data
func processCustomer(inputPath, outputDir string) error {
	log.Printf("Processing customer.go file: %s", inputPath)

	// Extract struct definitions
	structInfos, err := extractStructDefs(inputPath)
	if err != nil {
		return fmt.Errorf("failed to extract structs: %v", err)
	}

	// Check if Customer struct exists
	if _, exists := structInfos["Customer"]; !exists {
		return fmt.Errorf("Customer struct not found in %s", inputPath)
	}

	// Generate data for Customer struct
	rows, err := generateCustomer(structInfos)
	if err != nil {
		return fmt.Errorf("failed to generate customer data: %v", err)
	}

	// Create output filename
	outputPath := filepath.Join(outputDir, "Customer_output.xlsx")

	// Save to Excel
	err = SaveToExcel("Customer", rows, outputPath)
	if err != nil {
		return fmt.Errorf("failed to save Excel: %v", err)
	}

	log.Printf("Generated Excel file: %s", outputPath)
	return nil
}

func main() {
	log.Println("Starting data generator")

	// Check command-line arguments
	if len(os.Args) < 2 || os.Args[1] != "customer" {
		log.Fatal("Usage: go run main.go customer")
	}

	// Ensure input directory exists
	if _, err := os.Stat(inputDir); os.IsNotExist(err) {
		log.Fatalf("Input directory does not exist: %s", inputDir)
	}

	// Ensure output directory exists
	log.Printf("Creating output directory: %s", outputDir)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	// Process customer.go
	inputPath := filepath.Join(inputDir, "customer.go")
	if _, err := os.Stat(inputPath); os.IsNotExist(err) {
		log.Fatalf("customer.go not found in input directory: %s", inputDir)
	}

	err := processCustomer(inputPath, outputDir)
	if err != nil {
		log.Fatalf("Error processing customer: %v", err)
	}

	log.Println("Processing completed successfully")
}
