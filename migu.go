package migu

import (
	"database/sql"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/naoina/go-stringutil"
	"github.com/naoina/migu/dialect"
)

const (
	commentPrefix       = "//"
	marker              = "+migu"
	annotationSeparator = ':'
	defaultVarcharSize  = 255
)

var (
	sameTypeMap = func() map[string][]string {
		m := map[string][]string{}
		for _, types := range [][]string{
			{"*int8", "*bool", "sql.NullBool"},
			{"int8", "bool"},
			{"*uint", "*uint32"},
			{"uint", "uint32"},
			{"*int", "*int32"},
			{"int", "int32"},
			{"*int64", "sql.NullInt64"},
			{"*string", "sql.NullString"},
			{"*float32", "*float64", "sql.NullFloat64"},
			{"float32", "float64"},
		} {
			for _, t := range types {
				m[t] = types
			}
		}
		return m
	}()
)

// Sync synchronizes the schema between Go's struct and the database.
// Go's struct may be provided via the filename of the source file, or via
// the src parameter.
//
// If src != nil, Sync parses the source from src and filename is not used.
// The type of the argument for the src parameter must be string, []byte, or
// io.Reader. If src == nil, Sync parses the file specified by filename.
//
// All query for synchronization will be performed within the transaction if
// storage engine supports the transaction. (e.g. MySQL's MyISAM engine does
// NOT support the transaction)
func Sync(db *sql.DB, filename string, src interface{}) error {
	sqls, err := Diff(db, filename, src)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, sql := range sqls {
		if _, err := tx.Exec(sql); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// Diff returns SQLs for schema synchronous between database and Go's struct.
func Diff(db *sql.DB, filename string, src interface{}) ([]string, error) {
	structASTMap, err := makeStructASTMap(filename, src)
	if err != nil {
		return nil, err
	}
	structMap := map[string]*table{}
	for name, structAST := range structASTMap {
		for _, fld := range structAST.StructType.Fields.List {
			typeName, err := detectTypeName(fld)
			if err != nil {
				return nil, err
			}
			f, err := newField(typeName, fld)
			if err != nil {
				return nil, err
			}
			if f.Ignore {
				continue
			}
			if !(ast.IsExported(f.Name) || (f.Name == "_" && f.Name != f.Column)) {
				continue
			}
			if structMap[name] == nil {
				structMap[name] = &table{
					Option: structAST.Annotation.Option,
				}
			}
			structMap[name].Fields = append(structMap[name].Fields, f)
		}
	}
	tableMap, err := getTableMap(db)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(structMap))
	for name := range structMap {
		names = append(names, name)
	}
	sort.Strings(names)
	d := &dialect.MySQL{}
	var migrations []string
	var oldFields []*field
	droppedColumn := map[string]struct{}{}
	for _, name := range names {
		tbl := structMap[name]
		tableName := d.Quote(name)
		if columns, ok := tableMap[name]; ok {
			for _, c := range columns {
				oldFieldAST, err := c.fieldAST()
				if err != nil {
					return nil, err
				}
				f, err := newField(fmt.Sprint(oldFieldAST.Type), oldFieldAST)
				if err != nil {
					return nil, err
				}
				oldFields = append(oldFields, f)
			}
			fields := makeAlterTableFields(oldFields, tbl.Fields)
			for _, f := range fields {
				switch {
				case f.IsAdded():
					migrations = append(migrations, fmt.Sprintf("ALTER TABLE %s ADD %s", tableName, d.Quote(f.new.Column)))
				case f.IsDropped():
					migrations = append(migrations, fmt.Sprintf("ALTER TABLE %s DROP %s", tableName, d.Quote(f.old.Column)))
				case f.IsModified():
					specs := make([]string, 0, 1)
					if f.old.PrimaryKey != f.new.PrimaryKey && !f.new.PrimaryKey {
						specs = append(specs, "DROP PRIMARY KEY")
					}
					specs = append(specs, fmt.Sprintf("MODIFY %s", columnSQL(d, f.new)))
					migrations = append(migrations, fmt.Sprintf("ALTER TABLE %s %s", tableName, strings.Join(specs, ", ")))
				}
			}
			for _, f := range fields {
				if f.IsDropped() {
					droppedColumn[f.old.Column] = struct{}{}
				}
			}
		} else {
			columns := make([]string, len(tbl.Fields))
			for i, f := range tbl.Fields {
				columns[i] = columnSQL(d, f)
			}
			query := fmt.Sprintf("CREATE TABLE %s (\n"+
				"  %s\n"+
				")", tableName, strings.Join(columns, ",\n  "))
			if tbl.Option != "" {
				query += " " + tbl.Option
			}
			migrations = append(migrations, query)
		}
		addIndexMap, dropIndexMap := makeIndexMap(oldFields, tbl.Fields)
		for name, index := range dropIndexMap {
			// If the column which has the index will be deleted, Migu will not delete the index related to the column
			// because the index will be deleted when the column which related to the index will be deleted.
			if _, ok := droppedColumn[index.Columns[0]]; !ok {
				migrations = append(migrations, fmt.Sprintf("DROP INDEX %s ON %s", d.Quote(name), tableName))
			}
		}
		for name, index := range addIndexMap {
			columns := make([]string, 0, len(index.Columns))
			for _, c := range index.Columns {
				columns = append(columns, d.Quote(c))
			}
			if index.Unique {
				migrations = append(migrations, fmt.Sprintf("CREATE UNIQUE INDEX %s ON %s (%s)", d.Quote(name), tableName, strings.Join(columns, ",")))
			} else {
				migrations = append(migrations, fmt.Sprintf("CREATE INDEX %s ON %s (%s)", d.Quote(name), tableName, strings.Join(columns, ",")))
			}
		}
		delete(structMap, name)
		delete(tableMap, name)
	}
	for name := range tableMap {
		migrations = append(migrations, fmt.Sprintf(`DROP TABLE %s`, d.Quote(name)))
	}
	return migrations, nil
}

type table struct {
	Fields []*field
	Option string
}

type index struct {
	Columns []string
	Unique  bool
}

type field struct {
	Name          string
	Type          string
	Column        string
	Comment       string
	RawIndexes    []string
	Unique        bool
	PrimaryKey    bool
	AutoIncrement bool
	Ignore        bool
	Default       string
	Size          uint64
}

func newField(typeName string, f *ast.Field) (*field, error) {
	ret := &field{
		Name: f.Names[0].Name,
		Type: typeName,
		Size: defaultVarcharSize,
	}
	if f.Tag != nil {
		s, err := strconv.Unquote(f.Tag.Value)
		if err != nil {
			return nil, err
		}
		if err := parseStructTag(ret, reflect.StructTag(s)); err != nil {
			return nil, err
		}
	}
	if f.Comment != nil {
		ret.Comment = strings.TrimSpace(f.Comment.Text())
	}
	if ret.Column == "" {
		ret.Column = stringutil.ToSnakeCase(ret.Name)
	}
	return ret, nil
}

func (f *field) Indexes() []string {
	indexes := make([]string, 0, len(f.RawIndexes))
	for _, index := range f.RawIndexes {
		if index == "" {
			index = f.Column
		}
		indexes = append(indexes, index)
	}
	return indexes
}

func (f *field) UniqueIndexes() []string {
	if !f.Unique {
		return nil
	}
	return []string{f.Column}
}

func (f *field) IsDifferent(another *field) bool {
	if f == nil && another == nil {
		return false
	}
	return ((f == nil && another != nil) || (f != nil && another == nil)) ||
		!isSameType(f.Type, another.Type) ||
		f.Default != another.Default ||
		f.Size != another.Size ||
		f.Column != another.Column ||
		f.Comment != another.Comment ||
		f.AutoIncrement != another.AutoIncrement ||
		f.PrimaryKey != another.PrimaryKey
}

func makeIndexMap(oldFields, newFields []*field) (addIndexMap, dropIndexMap map[string]*index) {
	dropIndexMap = map[string]*index{}
	addIndexMap = map[string]*index{}
	m := make(map[string]*field, len(oldFields))
	for _, f := range oldFields {
		m[f.Column] = f
	}
	for _, f := range newFields {
		oldField := m[f.Column]
		if oldField == nil {
			oldField = &field{}
		}
		oindexes, nindexes := oldField.Indexes(), f.Indexes()
		oldUniqueIndexes, newUniqueIndexes := oldField.UniqueIndexes(), f.UniqueIndexes()
		for _, name := range oindexes {
			if !inStrings(nindexes, name) {
				if dropIndexMap[name] == nil {
					dropIndexMap[name] = &index{Unique: false}
				}
				dropIndexMap[name].Columns = append(dropIndexMap[name].Columns, oldField.Column)
			}
		}
		for _, name := range oldUniqueIndexes {
			if !inStrings(newUniqueIndexes, name) {
				if dropIndexMap[name] == nil {
					dropIndexMap[name] = &index{Unique: true}
				}
				dropIndexMap[name].Columns = append(dropIndexMap[name].Columns, oldField.Column)
			}
		}
		for _, name := range nindexes {
			if !inStrings(oindexes, name) {
				if addIndexMap[name] == nil {
					addIndexMap[name] = &index{Unique: false}
				}
				addIndexMap[name].Columns = append(addIndexMap[name].Columns, f.Column)
			}
		}
		for _, name := range newUniqueIndexes {
			if !inStrings(oldUniqueIndexes, name) {
				if addIndexMap[name] == nil {
					addIndexMap[name] = &index{Unique: true}
				}
				addIndexMap[name].Columns = append(addIndexMap[name].Columns, f.Column)
			}
		}
	}
	return addIndexMap, dropIndexMap
}

type modifiedField struct {
	old *field
	new *field
}

func (f *modifiedField) IsAdded() bool {
	return f.old == nil && f.new != nil
}

func (f *modifiedField) IsDropped() bool {
	return f.old != nil && f.new == nil
}

func (f *modifiedField) IsModified() bool {
	return f.old != nil && f.new != nil
}

func makeAlterTableFields(oldFields, newFields []*field) (fields []modifiedField) {
	oldTable := make(map[string]*field, len(oldFields))
	for _, f := range oldFields {
		oldTable[f.Column] = f
	}
	newTable := make(map[string]*field, len(newFields))
	for _, f := range newFields {
		newTable[f.Column] = f
	}
	for _, f := range newFields {
		if oldF := oldTable[f.Column]; oldF.IsDifferent(f) {
			fields = append(fields, modifiedField{
				old: oldF,
				new: f,
			})
		}
	}
	for _, f := range oldFields {
		newField := newTable[f.Column]
		if newField == nil {
			fields = append(fields, modifiedField{
				old: f,
				new: nil,
			})
		}
	}
	return fields
}

// Fprint generates Go's structs from database schema and writes to output.
func Fprint(output io.Writer, db *sql.DB) error {
	tableMap, err := getTableMap(db)
	if err != nil {
		return err
	}
	if hasDatetimeColumn(tableMap) {
		if err := fprintln(output, importAST("time")); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(tableMap))
	for name := range tableMap {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s, err := makeStructAST(name, tableMap[name])
		if err != nil {
			return err
		}
		if err := fprintln(output, s); err != nil {
			return err
		}
	}
	return nil
}

const (
	tagDefault       = "default"
	tagPrimaryKey    = "pk"
	tagAutoIncrement = "autoincrement"
	tagIndex         = "index"
	tagUnique        = "unique"
	tagSize          = "size"
	tagColumn        = "column"
	tagIgnore        = "-"
)

func getTableMap(db *sql.DB) (map[string][]*columnSchema, error) {
	dbname, err := getCurrentDBName(db)
	if err != nil {
		return nil, err
	}
	indexMap, err := getIndexMap(db, dbname)
	if err != nil {
		return nil, err
	}
	query := strings.Join([]string{
		"SELECT",
		"  TABLE_NAME,",
		"  COLUMN_NAME,",
		"  COLUMN_DEFAULT,",
		"  IS_NULLABLE,",
		"  DATA_TYPE,",
		"  CHARACTER_MAXIMUM_LENGTH,",
		"  CHARACTER_OCTET_LENGTH,",
		"  NUMERIC_PRECISION,",
		"  NUMERIC_SCALE,",
		"  COLUMN_TYPE,",
		"  COLUMN_KEY,",
		"  EXTRA,",
		"  COLUMN_COMMENT",
		"FROM information_schema.COLUMNS",
		"WHERE TABLE_SCHEMA = ?",
		"ORDER BY TABLE_NAME, ORDINAL_POSITION",
	}, "\n")
	rows, err := db.Query(query, dbname)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tableMap := map[string][]*columnSchema{}
	for rows.Next() {
		schema := &columnSchema{}
		if err := rows.Scan(
			&schema.TableName,
			&schema.ColumnName,
			&schema.ColumnDefault,
			&schema.IsNullable,
			&schema.DataType,
			&schema.CharacterMaximumLength,
			&schema.CharacterOctetLength,
			&schema.NumericPrecision,
			&schema.NumericScale,
			&schema.ColumnType,
			&schema.ColumnKey,
			&schema.Extra,
			&schema.ColumnComment,
		); err != nil {
			return nil, err
		}
		tableMap[schema.TableName] = append(tableMap[schema.TableName], schema)
		if tableIndex, exists := indexMap[schema.TableName]; exists {
			if info, exists := tableIndex[schema.ColumnName]; exists {
				schema.NonUnique = info.NonUnique
				schema.IndexName = info.IndexName
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tableMap, nil
}

func getCurrentDBName(db *sql.DB) (string, error) {
	var dbname sql.NullString
	err := db.QueryRow(`SELECT DATABASE()`).Scan(&dbname)
	return dbname.String, err
}

type indexInfo struct {
	NonUnique int64
	IndexName string
}

func getIndexMap(db *sql.DB, dbname string) (map[string]map[string]indexInfo, error) {
	query := strings.Join([]string{
		"SELECT",
		"  TABLE_NAME,",
		"  COLUMN_NAME,",
		"  NON_UNIQUE,",
		"  INDEX_NAME",
		"FROM information_schema.STATISTICS",
		"WHERE TABLE_SCHEMA = ?",
	}, "\n")
	rows, err := db.Query(query, dbname)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	indexMap := make(map[string]map[string]indexInfo)
	for rows.Next() {
		var (
			tableName  string
			columnName string
			index      indexInfo
		)
		if err := rows.Scan(&tableName, &columnName, &index.NonUnique, &index.IndexName); err != nil {
			return nil, err
		}
		if _, exists := indexMap[tableName]; !exists {
			indexMap[tableName] = make(map[string]indexInfo)
		}
		indexMap[tableName][columnName] = index
	}
	return indexMap, rows.Err()
}

func formatDefault(d dialect.Dialect, t, def string) string {
	switch t {
	case "string":
		return d.QuoteString(def)
	default:
		return def
	}
}

func fprintln(output io.Writer, decl ast.Decl) error {
	fmt.Fprintln(output, commentPrefix+marker)
	if err := format.Node(output, token.NewFileSet(), decl); err != nil {
		return err
	}
	fmt.Fprintf(output, "\n\n")
	return nil
}

type structAST struct {
	StructType *ast.StructType
	Annotation *annotation
}

func makeStructASTMap(filename string, src interface{}) (map[string]*structAST, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	structASTMap := map[string]*structAST{}
	for _, decl := range f.Decls {
		d, ok := decl.(*ast.GenDecl)
		if !ok || d.Tok != token.TYPE || d.Doc == nil {
			continue
		}
		annotation, err := parseAnnotation(d.Doc)
		if err != nil {
			return nil, err
		}
		if annotation == nil {
			continue
		}
		for _, spec := range d.Specs {
			s, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			t, ok := s.Type.(*ast.StructType)
			if !ok {
				continue
			}
			st := &structAST{
				StructType: t,
				Annotation: annotation,
			}
			if annotation.Table != "" {
				structASTMap[annotation.Table] = st
			} else {
				structASTMap[stringutil.ToSnakeCase(s.Name.Name)] = st
			}
		}
	}
	return structASTMap, nil
}

func detectTypeName(n ast.Node) (string, error) {
	switch t := n.(type) {
	case *ast.Field:
		return detectTypeName(t.Type)
	case *ast.Ident:
		return t.Name, nil
	case *ast.SelectorExpr:
		name, err := detectTypeName(t.X)
		if err != nil {
			return "", err
		}
		return name + "." + t.Sel.Name, nil
	case *ast.StarExpr:
		name, err := detectTypeName(t.X)
		if err != nil {
			return "", err
		}
		return "*" + name, nil
	default:
		return "", fmt.Errorf("migu: BUG: unknown type %T", t)
	}
}

func columnSQL(d dialect.Dialect, f *field) string {
	colType, null := d.ColumnType(f.Type, f.Size, f.AutoIncrement)
	column := []string{d.Quote(f.Column), colType}
	if !null {
		column = append(column, "NOT NULL")
	}
	if f.Default != "" {
		column = append(column, "DEFAULT", formatDefault(d, f.Type, f.Default))
	}
	if f.PrimaryKey {
		column = append(column, "PRIMARY KEY")
	}
	if f.AutoIncrement && d.AutoIncrement() != "" {
		column = append(column, d.AutoIncrement())
	}
	if f.Comment != "" {
		column = append(column, "COMMENT", d.QuoteString(f.Comment))
	}
	return strings.Join(column, " ")
}

func hasDatetimeColumn(t map[string][]*columnSchema) bool {
	for _, schemas := range t {
		for _, schema := range schemas {
			if schema.DataType == "datetime" {
				return true
			}
		}
	}
	return false
}

func importAST(pkg string) ast.Decl {
	return &ast.GenDecl{
		Tok: token.IMPORT,
		Specs: []ast.Spec{
			&ast.ImportSpec{
				Path: &ast.BasicLit{
					Kind:  token.STRING,
					Value: fmt.Sprintf(`"%s"`, pkg),
				},
			},
		},
	}
}

func makeStructAST(name string, schemas []*columnSchema) (ast.Decl, error) {
	var fields []*ast.Field
	for _, schema := range schemas {
		f, err := schema.fieldAST()
		if err != nil {
			return nil, err
		}
		fields = append(fields, f)
	}
	return &ast.GenDecl{
		Tok: token.TYPE,
		Specs: []ast.Spec{
			&ast.TypeSpec{
				Name: ast.NewIdent(stringutil.ToUpperCamelCase(name)),
				Type: &ast.StructType{
					Fields: &ast.FieldList{
						List: fields,
					},
				},
			},
		},
	}, nil
}

func parseStructTag(f *field, tag reflect.StructTag) error {
	migu := tag.Get("migu")
	if migu == "" {
		return nil
	}
	for _, opt := range strings.Split(migu, ",") {
		optval := strings.SplitN(opt, ":", 2)
		switch optval[0] {
		case tagDefault:
			if len(optval) > 1 {
				f.Default = optval[1]
			}
		case tagPrimaryKey:
			f.PrimaryKey = true
		case tagAutoIncrement:
			f.AutoIncrement = true
		case tagIndex:
			if len(optval) == 2 {
				f.RawIndexes = append(f.RawIndexes, optval[1])
			} else {
				f.RawIndexes = append(f.RawIndexes, "")
			}
		case tagUnique:
			f.Unique = true
		case tagIgnore:
			f.Ignore = true
		case tagColumn:
			if len(optval) < 2 {
				return fmt.Errorf("`column` tag must specify the parameter")
			}
			f.Column = optval[1]
		case tagSize:
			if len(optval) < 2 {
				return fmt.Errorf("`size' tag must specify the parameter")
			}
			size, err := strconv.ParseUint(optval[1], 10, 64)
			if err != nil {
				return err
			}
			f.Size = size
		default:
			return fmt.Errorf("unknown option: `%s'", opt)
		}
	}
	return nil
}

func isSameType(t1, t2 string) bool {
	return t1 == t2 || inStrings(sameTypeMap[t1], t2)
}

type columnSchema struct {
	TableName              string
	ColumnName             string
	OrdinalPosition        int64
	ColumnDefault          sql.NullString
	IsNullable             string
	DataType               string
	CharacterMaximumLength *uint64
	CharacterOctetLength   sql.NullInt64
	NumericPrecision       sql.NullInt64
	NumericScale           sql.NullInt64
	ColumnType             string
	ColumnKey              string
	Extra                  string
	ColumnComment          string
	NonUnique              int64
	IndexName              string
}

func (schema *columnSchema) fieldAST() (*ast.Field, error) {
	types, err := schema.GoFieldTypes()
	if err != nil {
		return nil, err
	}
	field := &ast.Field{
		Names: []*ast.Ident{
			ast.NewIdent(stringutil.ToUpperCamelCase(schema.ColumnName)),
		},
		Type: ast.NewIdent(types[0]),
	}
	var tags []string
	if schema.ColumnDefault.Valid {
		tags = append(tags, tagDefault+":"+schema.ColumnDefault.String)
	}
	if schema.hasPrimaryKey() {
		tags = append(tags, tagPrimaryKey)
	}
	if schema.hasAutoIncrement() {
		tags = append(tags, tagAutoIncrement)
	}
	if schema.hasIndex() {
		if schema.IndexName == schema.ColumnName {
			tags = append(tags, tagIndex)
		} else {
			tags = append(tags, fmt.Sprintf("%s:%s", tagIndex, schema.IndexName))
		}
	}
	if schema.hasUniqueKey() {
		tags = append(tags, tagUnique)
	}
	if schema.hasSize() {
		if *schema.CharacterMaximumLength != defaultVarcharSize {
			tags = append(tags, fmt.Sprintf("%s:%d", tagSize, *schema.CharacterMaximumLength))
		}
	}
	if len(tags) > 0 {
		field.Tag = &ast.BasicLit{
			Kind:  token.STRING,
			Value: fmt.Sprintf("`migu:\"%s\"`", strings.Join(tags, ",")),
		}
	}
	if schema.ColumnComment != "" {
		field.Comment = &ast.CommentGroup{
			List: []*ast.Comment{
				{Text: " // " + schema.ColumnComment},
			},
		}
	}
	return field, nil
}

func (schema *columnSchema) GoFieldTypes() ([]string, error) {
	switch schema.DataType {
	case "tinyint":
		if schema.isUnsigned() {
			if schema.isNullable() {
				return []string{"*uint8"}, nil
			}
			return []string{"uint8"}, nil
		}
		if schema.isNullable() {
			return []string{"*int8", "*bool", "sql.NullBool"}, nil
		}
		return []string{"int8", "bool"}, nil
	case "smallint":
		if schema.isUnsigned() {
			if schema.isNullable() {
				return []string{"*uint16"}, nil
			}
			return []string{"uint16"}, nil
		}
		if schema.isNullable() {
			return []string{"*int16"}, nil
		}
		return []string{"int16"}, nil
	case "mediumint", "int":
		if schema.isUnsigned() {
			if schema.isNullable() {
				return []string{"*uint", "*uint32"}, nil
			}
			return []string{"uint", "uint32"}, nil
		}
		if schema.isNullable() {
			return []string{"*int", "*int32"}, nil
		}
		return []string{"int", "int32"}, nil
	case "bigint":
		if schema.isUnsigned() {
			if schema.isNullable() {
				return []string{"*uint64"}, nil
			}
			return []string{"uint64"}, nil
		}
		if schema.isNullable() {
			return []string{"*int64", "sql.NullInt64"}, nil
		}
		return []string{"int64"}, nil
	case "varchar", "text", "mediumtext", "longtext":
		if schema.isNullable() {
			return []string{"*string", "sql.NullString"}, nil
		}
		return []string{"string"}, nil
	case "datetime":
		if schema.isNullable() {
			return []string{"*time.Time"}, nil
		}
		return []string{"time.Time"}, nil
	case "double":
		if schema.isNullable() {
			return []string{"*float32", "*float64", "sql.NullFloat64"}, nil
		}
		return []string{"float32", "float64"}, nil
	default:
		return nil, fmt.Errorf("BUG: unexpected data type: %s", schema.DataType)
	}
}

func (schema *columnSchema) isUnsigned() bool {
	return strings.Contains(schema.ColumnType, "unsigned")
}

func (schema *columnSchema) isNullable() bool {
	return strings.ToUpper(schema.IsNullable) == "YES"
}

func (schema *columnSchema) hasPrimaryKey() bool {
	return schema.ColumnKey == "PRI" && strings.ToUpper(schema.IndexName) == "PRIMARY"
}

func (schema *columnSchema) hasAutoIncrement() bool {
	return schema.Extra == "auto_increment"
}

func (schema *columnSchema) hasIndex() bool {
	return schema.IndexName != "" && !schema.hasPrimaryKey() && schema.NonUnique != 0
}

func (schema *columnSchema) hasUniqueKey() bool {
	return schema.IndexName != "" && !schema.hasPrimaryKey() && schema.NonUnique == 0
}

func (schema *columnSchema) hasSize() bool {
	return schema.DataType == "varchar" && schema.CharacterMaximumLength != nil
}
