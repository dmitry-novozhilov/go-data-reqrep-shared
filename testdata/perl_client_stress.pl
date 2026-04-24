#!/usr/bin/env perl
use strict;
use warnings;
use Data::ReqRep::Shared::Client;

my ($path, @sizes) = @ARGV;
die "usage: perl_client_stress.pl <path> <size>...\n"
    unless defined $path && @sizes;

my $cli = Data::ReqRep::Shared::Client->new($path);

for my $n (@sizes) {
    my $msg  = 'x' x $n;
    my $resp = $cli->req_wait($msg, 10.0);
    if (!defined $resp) {
        print "FAIL timeout $n\n";
        next;
    }
    if (length($resp) == $n && $resp eq $msg) {
        print "ok $n\n";
    } else {
        printf "FAIL mismatch %d got_len=%d\n", $n, length($resp);
    }
}
