/* C ABI over the Xapian C++ API — the minimal surface the flatcurve engine
 * needs. Every call reports failure through err_out (malloc'd message the
 * caller frees); handles are opaque pointers owned by the caller until the
 * matching *_free / *_close. */
#ifndef YARILO_FTS_FLATCURVE_SHIM_H
#define YARILO_FTS_FLATCURVE_SHIM_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef void fcx_wdb;   /* Xapian::WritableDatabase */
typedef void fcx_db;    /* Xapian::Database (combined reader) */
typedef void fcx_doc;   /* Xapian::Document */
typedef void fcx_query; /* Xapian::Query */
typedef void fcx_mset;  /* Xapian::MSet */

/* --- writable database ------------------------------------------------- */
fcx_wdb *fcx_wdb_open(const char *path, char **err_out);
int fcx_wdb_commit(fcx_wdb *w, char **err_out);
void fcx_wdb_close(fcx_wdb *w);
int fcx_wdb_replace_document(fcx_wdb *w, unsigned int docid, fcx_doc *d,
                             char **err_out);
/* existed_out: 1 when the document was present. DocNotFound is not an error. */
int fcx_wdb_delete_document(fcx_wdb *w, unsigned int docid, int *existed_out,
                            char **err_out);
int fcx_wdb_set_metadata(fcx_wdb *w, const char *key, const char *value,
                         char **err_out);
char *fcx_wdb_get_metadata(fcx_wdb *w, const char *key, char **err_out);
unsigned int fcx_wdb_get_doccount(fcx_wdb *w, char **err_out);
int fcx_wdb_doc_exists(fcx_wdb *w, unsigned int docid, char **err_out);

/* --- combined read-only database ---------------------------------------- */
fcx_db *fcx_db_open_multi(const char *const *paths, size_t n, char **err_out);
void fcx_db_close(fcx_db *db);
unsigned int fcx_db_get_lastdocid(fcx_db *db, char **err_out);
unsigned int fcx_db_get_doccount(fcx_db *db, char **err_out);
/* Fills up to cap docids starting after prev (0 = from the beginning);
 * returns the count written, -1 on error. Iteration order is ascending. */
int fcx_db_docids(fcx_db *db, unsigned int prev, unsigned int *buf,
                  size_t cap, char **err_out);
int fcx_db_compact(fcx_db *db, const char *dest, char **err_out);

/* --- document ------------------------------------------------------------ */
fcx_doc *fcx_doc_new(void);
void fcx_doc_free(fcx_doc *d);
int fcx_doc_add_term(fcx_doc *d, const char *term, char **err_out);
int fcx_doc_add_boolean_term(fcx_doc *d, const char *term, char **err_out);

/* --- query --------------------------------------------------------------- */
/* op values mirror Xapian::Query::op */
enum {
	FCX_OP_AND = 0,
	FCX_OP_OR = 1,
	FCX_OP_AND_NOT = 2
};
fcx_query *fcx_query_wildcard(const char *pattern, char **err_out);
fcx_query *fcx_query_term(const char *term, char **err_out);
fcx_query *fcx_query_match_all(char **err_out);
fcx_query *fcx_query_combine(int op, fcx_query *a, fcx_query *b,
                             char **err_out);
void fcx_query_free(fcx_query *q);

/* --- search --------------------------------------------------------------- */
fcx_mset *fcx_db_search(fcx_db *db, fcx_query *q, char **err_out);
size_t fcx_mset_size(fcx_mset *m);
/* idx < fcx_mset_size(); weight_out may be NULL. */
unsigned int fcx_mset_docid(fcx_mset *m, size_t idx, double *weight_out);
void fcx_mset_free(fcx_mset *m);

#ifdef __cplusplus
}
#endif

#endif
