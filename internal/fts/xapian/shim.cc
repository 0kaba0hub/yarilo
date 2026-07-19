//go:build flatcurve

#include "shim.h"

#include <xapian.h>

#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

namespace {

char *dup_error(const std::string &msg) {
	char *out = static_cast<char *>(malloc(msg.size() + 1));
	if (out != nullptr)
		memcpy(out, msg.c_str(), msg.size() + 1);
	return out;
}

void set_err(char **err_out, const std::string &msg) {
	if (err_out != nullptr)
		*err_out = dup_error(msg);
}

#define FCX_TRY try
#define FCX_CATCH(errval)                                                      \
	catch (const Xapian::Error &e) {                                       \
		set_err(err_out, e.get_description());                         \
		return errval;                                                 \
	}                                                                      \
	catch (const std::exception &e) {                                     \
		set_err(err_out, e.what());                                    \
		return errval;                                                 \
	}

} // namespace

extern "C" {

/* --- writable database ------------------------------------------------- */

fcx_wdb *fcx_wdb_open(const char *path, char **err_out) {
	FCX_TRY {
		return new Xapian::WritableDatabase(
			path, Xapian::DB_CREATE_OR_OPEN | Xapian::DB_NO_SYNC);
	}
	FCX_CATCH(nullptr)
}

int fcx_wdb_commit(fcx_wdb *w, char **err_out) {
	FCX_TRY {
		static_cast<Xapian::WritableDatabase *>(w)->commit();
		return 0;
	}
	FCX_CATCH(-1)
}

void fcx_wdb_close(fcx_wdb *w) {
	delete static_cast<Xapian::WritableDatabase *>(w);
}

int fcx_wdb_replace_document(fcx_wdb *w, unsigned int docid, fcx_doc *d,
                             char **err_out) {
	FCX_TRY {
		static_cast<Xapian::WritableDatabase *>(w)->replace_document(
			docid, *static_cast<Xapian::Document *>(d));
		return 0;
	}
	FCX_CATCH(-1)
}

int fcx_wdb_delete_document(fcx_wdb *w, unsigned int docid, int *existed_out,
                            char **err_out) {
	if (existed_out != nullptr)
		*existed_out = 0;
	FCX_TRY {
		Xapian::WritableDatabase *wdb =
			static_cast<Xapian::WritableDatabase *>(w);
		try {
			wdb->get_document(docid);
		} catch (const Xapian::DocNotFoundError &) {
			return 0;
		}
		wdb->delete_document(docid);
		if (existed_out != nullptr)
			*existed_out = 1;
		return 0;
	}
	FCX_CATCH(-1)
}

int fcx_wdb_set_metadata(fcx_wdb *w, const char *key, const char *value,
                         char **err_out) {
	FCX_TRY {
		static_cast<Xapian::WritableDatabase *>(w)->set_metadata(key,
									 value);
		return 0;
	}
	FCX_CATCH(-1)
}

char *fcx_wdb_get_metadata(fcx_wdb *w, const char *key, char **err_out) {
	FCX_TRY {
		std::string v =
			static_cast<Xapian::WritableDatabase *>(w)->get_metadata(
				key);
		return dup_error(v); /* plain malloc'd copy */
	}
	FCX_CATCH(nullptr)
}

unsigned int fcx_wdb_get_doccount(fcx_wdb *w, char **err_out) {
	FCX_TRY {
		return static_cast<Xapian::WritableDatabase *>(w)
			->get_doccount();
	}
	FCX_CATCH(0)
}

int fcx_wdb_doc_exists(fcx_wdb *w, unsigned int docid, char **err_out) {
	FCX_TRY {
		try {
			static_cast<Xapian::WritableDatabase *>(w)->get_document(
				docid);
			return 1;
		} catch (const Xapian::DocNotFoundError &) {
			return 0;
		}
	}
	FCX_CATCH(-1)
}

/* --- combined read-only database ---------------------------------------- */

fcx_db *fcx_db_open_multi(const char *const *paths, size_t n, char **err_out) {
	FCX_TRY {
		Xapian::Database *db = new Xapian::Database();
		try {
			for (size_t i = 0; i < n; i++)
				db->add_database(Xapian::Database(paths[i]));
		} catch (...) {
			delete db;
			throw;
		}
		return db;
	}
	FCX_CATCH(nullptr)
}

void fcx_db_close(fcx_db *db) { delete static_cast<Xapian::Database *>(db); }

unsigned int fcx_db_get_lastdocid(fcx_db *db, char **err_out) {
	FCX_TRY {
		return static_cast<Xapian::Database *>(db)->get_lastdocid();
	}
	FCX_CATCH(0)
}

unsigned int fcx_db_get_doccount(fcx_db *db, char **err_out) {
	FCX_TRY {
		return static_cast<Xapian::Database *>(db)->get_doccount();
	}
	FCX_CATCH(0)
}

int fcx_db_docids(fcx_db *db, unsigned int prev, unsigned int *buf, size_t cap,
                  char **err_out) {
	FCX_TRY {
		Xapian::Database *d = static_cast<Xapian::Database *>(db);
		Xapian::PostingIterator it = d->postlist_begin("");
		Xapian::PostingIterator end = d->postlist_end("");
		if (prev > 0)
			it.skip_to(prev + 1);
		size_t n = 0;
		for (; it != end && n < cap; ++it)
			buf[n++] = *it;
		return static_cast<int>(n);
	}
	FCX_CATCH(-1)
}

int fcx_db_compact(fcx_db *db, const char *dest, char **err_out) {
	FCX_TRY {
		static_cast<Xapian::Database *>(db)->compact(
			dest, Xapian::DBCOMPACT_NO_RENUMBER |
				      Xapian::DBCOMPACT_MULTIPASS |
				      Xapian::Compactor::FULLER);
		return 0;
	}
	FCX_CATCH(-1)
}

/* --- document ------------------------------------------------------------ */

fcx_doc *fcx_doc_new(void) { return new Xapian::Document(); }

void fcx_doc_free(fcx_doc *d) { delete static_cast<Xapian::Document *>(d); }

int fcx_doc_add_term(fcx_doc *d, const char *term, char **err_out) {
	FCX_TRY {
		static_cast<Xapian::Document *>(d)->add_term(term);
		return 0;
	}
	FCX_CATCH(-1)
}

int fcx_doc_add_boolean_term(fcx_doc *d, const char *term, char **err_out) {
	FCX_TRY {
		static_cast<Xapian::Document *>(d)->add_boolean_term(term);
		return 0;
	}
	FCX_CATCH(-1)
}

/* --- query --------------------------------------------------------------- */

fcx_query *fcx_query_wildcard(const char *pattern, char **err_out) {
	FCX_TRY {
		return new Xapian::Query(Xapian::Query::OP_WILDCARD, pattern);
	}
	FCX_CATCH(nullptr)
}

fcx_query *fcx_query_term(const char *term, char **err_out) {
	FCX_TRY { return new Xapian::Query(term); }
	FCX_CATCH(nullptr)
}

fcx_query *fcx_query_match_all(char **err_out) {
	FCX_TRY { return new Xapian::Query(Xapian::Query::MatchAll); }
	FCX_CATCH(nullptr)
}

fcx_query *fcx_query_combine(int op, fcx_query *a, fcx_query *b,
                             char **err_out) {
	FCX_TRY {
		Xapian::Query::op xop;
		switch (op) {
		case FCX_OP_AND:
			xop = Xapian::Query::OP_AND;
			break;
		case FCX_OP_OR:
			xop = Xapian::Query::OP_OR;
			break;
		case FCX_OP_AND_NOT:
			xop = Xapian::Query::OP_AND_NOT;
			break;
		default:
			set_err(err_out, "unknown query op");
			return nullptr;
		}
		return new Xapian::Query(xop,
					 *static_cast<Xapian::Query *>(a),
					 *static_cast<Xapian::Query *>(b));
	}
	FCX_CATCH(nullptr)
}

void fcx_query_free(fcx_query *q) {
	delete static_cast<Xapian::Query *>(q);
}

/* --- search --------------------------------------------------------------- */

fcx_mset *fcx_db_search(fcx_db *db, fcx_query *q, char **err_out) {
	FCX_TRY {
		Xapian::Database *d = static_cast<Xapian::Database *>(db);
		Xapian::Enquire enq(*d);
		enq.set_query(*static_cast<Xapian::Query *>(q));
		enq.set_docid_order(Xapian::Enquire::DONT_CARE);
		Xapian::MSet *m = new Xapian::MSet(
			enq.get_mset(0, d->get_doccount()));
		return m;
	}
	FCX_CATCH(nullptr)
}

size_t fcx_mset_size(fcx_mset *m) {
	return static_cast<Xapian::MSet *>(m)->size();
}

unsigned int fcx_mset_docid(fcx_mset *m, size_t idx, double *weight_out) {
	Xapian::MSet *ms = static_cast<Xapian::MSet *>(m);
	Xapian::MSetIterator it = (*ms)[idx];
	if (weight_out != nullptr)
		*weight_out = it.get_weight();
	return *it;
}

void fcx_mset_free(fcx_mset *m) { delete static_cast<Xapian::MSet *>(m); }

} /* extern "C" */
