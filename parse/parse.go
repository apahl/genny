package parse

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"strings"
	"unicode"

	"golang.org/x/tools/imports"
)

var header = []byte(`

// This file was automatically generated by genny.
// Any changes will be lost if this file is regenerated.
// see https://github.com/cheekybits/genny

`)

var (
	packageKeyword = []byte("package")
	importKeyword  = []byte("import")
	openBrace      = []byte("(")
	closeBrace     = []byte(")")
	genericPackage = "generic"
	genericType    = "generic.Type"
	genericNumber  = "generic.Number"
	linefeed       = "\r\n"
)
var unwantedLinePrefixes = [][]byte{
	[]byte("//go:generate genny "),
}

func subIntoLiteral(lit, typeTemplate, specificType string) string {
	if lit == typeTemplate {
		return specificType
	}
	if !strings.Contains(lit, typeTemplate) {
		return lit
	}
	specificLg := wordify(specificType, true)
	specificSm := wordify(specificType, false)
	result := strings.Replace(lit, typeTemplate, specificLg, -1)
	if strings.HasPrefix(result, specificLg) && !isExported(lit) {
		return strings.Replace(result, specificLg, specificSm, 1)
	}
	return result
}

func subTypeIntoComment(line, typeTemplate, specificType string) string {
	var subbed string
	for _, w := range strings.Fields(line) {
		subbed = subbed + subIntoLiteral(w, typeTemplate, specificType) + " "
	}
	return subbed
}

// Does the heavy lifting of taking a line of our code and
// sbustituting a type into there for our generic type
func subTypeIntoLine(line, typeTemplate, specificType string) string {
	src := []byte(line)
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	s.Init(file, src, nil, scanner.ScanComments)
	output := ""
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		} else if tok == token.COMMENT {
			subbed := subTypeIntoComment(lit, typeTemplate, specificType)
			output = output + subbed + " "
		} else if tok.IsLiteral() {
			subbed := subIntoLiteral(lit, typeTemplate, specificType)
			output = output + subbed + " "
		} else {
			output = output + tok.String() + " "
		}
	}
	return output
}

// typeSet looks like "KeyType: int, ValueType: string"
func generateSpecific(filename string, in io.ReadSeeker, typeSet map[string]string) ([]byte, error) {

	// ensure we are at the beginning of the file
	in.Seek(0, os.SEEK_SET)

	// parse the source file
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, filename, in, 0)
	if err != nil {
		return nil, &errSource{Err: err}
	}

	// make sure every generic.Type is represented in the types
	// argument.
	for _, decl := range file.Decls {
		switch it := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range it.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				switch tt := ts.Type.(type) {
				case *ast.SelectorExpr:
					if name, ok := tt.X.(*ast.Ident); ok {
						if name.Name == genericPackage {
							if _, ok := typeSet[ts.Name.Name]; !ok {
								return nil, &errMissingSpecificType{GenericType: ts.Name.Name}
							}
						}
					}
				}
			}
		}
	}

	in.Seek(0, os.SEEK_SET)

	var buf bytes.Buffer

	comment := ""
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {

		line := scanner.Text()

		// does this line contain generic.Type?
		if strings.Contains(line, genericType) || strings.Contains(line, genericNumber) {
			comment = ""
			continue
		}

		for t, specificType := range typeSet {
			if strings.Contains(line, t) {
				newLine := subTypeIntoLine(line, t, specificType)
				line = newLine
			}
		}

		if comment != "" {
			buf.WriteString(makeLine(comment))
			comment = ""
		}

		// is this line a comment?
		// TODO: should we handle /* */ comments?
		if strings.HasPrefix(line, "//") {
			// record this line to print later
			comment = line
			continue
		}

		// write the line
		buf.WriteString(makeLine(line))
	}

	// write it out
	return buf.Bytes(), nil
}

// Generics parses the source file and generates the bytes replacing the
// generic types for the keys map with the specific types (its value).
func Generics(filename, outputFilename, pkgName string, in io.ReadSeeker, typeSets []map[string]string) ([]byte, error) {

	totalOutput := header

	for _, typeSet := range typeSets {

		// generate the specifics
		parsed, err := generateSpecific(filename, in, typeSet)
		if err != nil {
			return nil, err
		}

		totalOutput = append(totalOutput, parsed...)

	}

	// clean up the code line by line
	packageFound := false
	insideImportBlock := false
	var cleanOutputLines []string
	scanner := bufio.NewScanner(bytes.NewReader(totalOutput))
	for scanner.Scan() {

		// end of imports block?
		if insideImportBlock {
			if bytes.HasSuffix(scanner.Bytes(), closeBrace) {
				insideImportBlock = false
			}
			continue
		}

		if bytes.HasPrefix(scanner.Bytes(), packageKeyword) {
			if packageFound {
				continue
			} else {
				packageFound = true
			}
		} else if bytes.HasPrefix(scanner.Bytes(), importKeyword) {
			if bytes.HasSuffix(scanner.Bytes(), openBrace) {
				insideImportBlock = true
			}
			continue
		}

		// check all unwantedLinePrefixes - and skip them
		skipline := false
		for _, prefix := range unwantedLinePrefixes {
			if bytes.HasPrefix(scanner.Bytes(), prefix) {
				skipline = true
				continue
			}
		}

		if skipline {
			continue
		}

		cleanOutputLines = append(cleanOutputLines, makeLine(scanner.Text()))
	}

	cleanOutput := strings.Join(cleanOutputLines, "")

	output := []byte(cleanOutput)
	var err error

	// change package name
	if pkgName != "" {
		output = changePackage(bytes.NewReader([]byte(output)), pkgName)
	}
	// fix the imports
	output, err = imports.Process(outputFilename, output, nil)
	if err != nil {
		return nil, &errImports{Err: err}
	}

	return output, nil
}

func makeLine(s string) string {
	return fmt.Sprintln(strings.TrimRight(s, linefeed))
}

// isAlphaNumeric gets whether the rune is alphanumeric or _.
func isAlphaNumeric(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// wordify turns a type into a nice word for function and type
// names etc.
func wordify(s string, exported bool) string {
	s = strings.TrimRight(s, "{}")
	s = strings.TrimLeft(s, "*&")
	s = strings.Replace(s, ".", "", -1)
	if !exported {
		return s
	}
	return strings.ToUpper(string(s[0])) + s[1:]
}

func changePackage(r io.Reader, pkgName string) []byte {
	var out bytes.Buffer
	sc := bufio.NewScanner(r)
	done := false

	for sc.Scan() {
		s := sc.Text()

		if !done && strings.HasPrefix(s, "package") {
			parts := strings.Split(s, " ")
			parts[1] = pkgName
			s = strings.Join(parts, " ")
			done = true
		}

		fmt.Fprintln(&out, s)
	}
	return out.Bytes()
}

func isExported(lit string) bool {
	if len(lit) == 0 {
		return false
	}
	return unicode.IsUpper(rune(lit[0]))
}
